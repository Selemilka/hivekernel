package runtime

import (
	"bufio"
	"context"
	"fmt"
	"github.com/selemilka/hivekernel/internal/hklog"
	"os/exec"
	"strings"
	"sync"
	"time"

	pb "github.com/selemilka/hivekernel/api/proto/hivepb"
	"github.com/selemilka/hivekernel/internal/process"
	"github.com/selemilka/hivekernel/internal/tracing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// RuntimeType identifies which runtime to use for an agent.
type RuntimeType string

const (
	RuntimePython RuntimeType = "python"
	RuntimeClaw   RuntimeType = "claw"
	RuntimeCustom RuntimeType = "custom"
)

// AgentRuntime represents a running agent runtime process.
type AgentRuntime struct {
	PID         process.PID
	Type        RuntimeType
	Addr        string // gRPC address agent is listening on
	ProcessInfo *process.Process
	Cmd         *exec.Cmd            // OS process handle (nil for virtual processes)
	Conn        *grpc.ClientConn     // gRPC client connection to agent
	Client      pb.AgentServiceClient // stub for Init/Shutdown/Heartbeat/Execute
	exitCh      chan struct{}         // closed when OS process exits (nil for virtual)
}

// Manager manages agent runtime processes (OS process spawn + gRPC connection).
type Manager struct {
	mu            sync.RWMutex
	runtimes      map[process.PID]*AgentRuntime
	coreAddr      string // core's gRPC address (agents connect back to this)
	pythonBin     string // path to python executable
	sdkPath       string // path to Python SDK directory (set as PYTHONPATH)
	clawBin       string // path to picoclaw binary (for RUNTIME_CLAW)
	onProcessExit func(pid process.PID, exitCode int) // called when OS process exits on its own
}

// NewManager creates a new runtime manager.
// coreAddr is the core gRPC address that agents will connect to.
// pythonBin is the path to the python executable (default: "python").
func NewManager(coreAddr string, pythonBin string) *Manager {
	if pythonBin == "" {
		pythonBin = "python"
	}
	return &Manager{
		runtimes:  make(map[process.PID]*AgentRuntime),
		coreAddr:  coreAddr,
		pythonBin: pythonBin,
	}
}

// SetSDKPath sets the Python SDK directory path. When set, PYTHONPATH is
// automatically injected into spawned agent processes.
func (m *Manager) SetSDKPath(path string) {
	m.sdkPath = path
}

// SetClawBin sets the path to the PicoClaw binary for RUNTIME_CLAW agents.
func (m *Manager) SetClawBin(path string) {
	m.clawBin = path
}

// OnProcessExit sets a callback invoked when an agent OS process exits on its own
// (auto-exit, crash, etc.) — NOT when StopRuntime() kills it explicitly.
func (m *Manager) OnProcessExit(fn func(pid process.PID, exitCode int)) {
	m.onProcessExit = fn
}

// StartRuntime launches an agent runtime for the given process.
// If proc has no RuntimeImage set, it registers a virtual runtime (no OS process).
// Otherwise it spawns a Python process, waits for READY, dials gRPC, and calls Init.
func (m *Manager) StartRuntime(proc *process.Process, rtType RuntimeType) (*AgentRuntime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.runtimes[proc.PID]; exists {
		return nil, fmt.Errorf("runtime already running for PID %d", proc.PID)
	}

	// Virtual process: no RuntimeImage — just register a map entry.
	if proc.RuntimeAddr == "" && proc.Name != "" {
		rt := &AgentRuntime{
			PID:         proc.PID,
			Type:        rtType,
			Addr:        fmt.Sprintf("virtual://pid-%d", proc.PID),
			ProcessInfo: proc,
		}
		m.runtimes[proc.PID] = rt
		proc.RuntimeAddr = rt.Addr
		hklog.For("runtime").Debug("registered virtual runtime", "pid", proc.PID, "name", proc.Name)
		return rt, nil
	}

	// Real process: spawn Python, wait for READY, dial gRPC, call Init.
	rt, err := m.spawnReal(proc, rtType)
	if err != nil {
		return nil, err
	}
	m.runtimes[proc.PID] = rt
	return rt, nil
}

// spawnReal dispatches to the appropriate runtime spawner based on type.
func (m *Manager) spawnReal(proc *process.Process, rtType RuntimeType) (*AgentRuntime, error) {
	runtimeImage := proc.RuntimeAddr // abused as runtime image spec before real addr is set
	if runtimeImage == "" {
		return nil, fmt.Errorf("no RuntimeImage for PID %d", proc.PID)
	}

	switch rtType {
	case RuntimePython:
		return m.spawnPython(proc, runtimeImage)
	case RuntimeClaw:
		return m.spawnClaw(proc, runtimeImage)
	default:
		return nil, fmt.Errorf("unsupported runtime type %q for PID %d", rtType, proc.PID)
	}
}

// spawnPython builds the command to launch a Python agent runtime.
func (m *Manager) spawnPython(proc *process.Process, runtimeImage string) (*AgentRuntime, error) {
	cmd := exec.Command(
		m.pythonBin,
		"-m", "hivekernel_sdk.runner",
		"--agent", runtimeImage,
		"--core", m.coreAddr,
	)
	env := cmd.Environ()
	env = append(env,
		fmt.Sprintf("HIVEKERNEL_PID=%d", proc.PID),
		fmt.Sprintf("HIVEKERNEL_CORE=%s", m.coreAddr),
	)
	if m.sdkPath != "" {
		env = append(env, fmt.Sprintf("PYTHONPATH=%s", m.sdkPath))
	}
	cmd.Env = env

	return m.launchAndConnect(proc, RuntimePython, cmd)
}

// spawnClaw builds the command to launch a PicoClaw agent runtime.
func (m *Manager) spawnClaw(proc *process.Process, runtimeImage string) (*AgentRuntime, error) {
	clawBin := m.resolveClawBinary(runtimeImage)
	cmd := exec.Command(clawBin, "hive", "--core", m.coreAddr)

	env := cmd.Environ()
	env = append(env,
		fmt.Sprintf("HIVEKERNEL_PID=%d", proc.PID),
		fmt.Sprintf("HIVEKERNEL_CORE=%s", m.coreAddr),
	)
	// Inject claw-specific env vars from metadata.
	for k, v := range proc.Metadata {
		if strings.HasPrefix(k, "claw.env.") {
			envKey := strings.TrimPrefix(k, "claw.env.")
			env = append(env, fmt.Sprintf("%s=%s", envKey, v))
		}
	}
	cmd.Env = env

	return m.launchAndConnect(proc, RuntimeClaw, cmd)
}

// resolveClawBinary determines the PicoClaw binary path.
// If runtimeImage is "picoclaw" (default), uses the configured clawBin path.
// Otherwise treats runtimeImage as an explicit path to the binary.
func (m *Manager) resolveClawBinary(runtimeImage string) string {
	if runtimeImage == "picoclaw" || runtimeImage == "" {
		if m.clawBin != "" {
			return m.clawBin
		}
		return "picoclaw" // hope it's on PATH
	}
	return runtimeImage
}

// launchAndConnect is the common logic for spawning any runtime process:
// starts the OS process, waits for READY <port>, dials gRPC, calls Init.
func (m *Manager) launchAndConnect(proc *process.Process, rtType RuntimeType, cmd *exec.Cmd) (*AgentRuntime, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe for PID %d: %w", proc.PID, err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start runtime for PID %d: %w", proc.PID, err)
	}

	// Wait for READY <port> on stdout (timeout 10s).
	readyCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "READY ") {
				readyCh <- strings.TrimPrefix(line, "READY ")
				return
			}
		}
		if err := scanner.Err(); err != nil {
			errCh <- fmt.Errorf("reading stdout: %w", err)
		} else {
			errCh <- fmt.Errorf("agent stdout closed without READY signal")
		}
	}()

	var port string
	select {
	case port = <-readyCh:
		// Got the port.
	case err := <-errCh:
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("runtime PID %d: %w", proc.PID, err)
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("runtime PID %d: READY timeout (10s)", proc.PID)
	}

	agentAddr := fmt.Sprintf("localhost:%s", strings.TrimSpace(port))
	hklog.For("runtime").Info("agent listening", "pid", proc.PID, "addr", agentAddr)

	// Dial gRPC to the agent.
	conn, err := grpc.NewClient(agentAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(tracing.ClientUnaryInterceptor()),
		grpc.WithChainStreamInterceptor(tracing.ClientStreamInterceptor()),
	)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("runtime PID %d: dial agent: %w", proc.PID, err)
	}

	client := pb.NewAgentServiceClient(conn)

	// Call Init with full process info.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	initResp, err := client.Init(ctx, &pb.InitRequest{
		Pid:           proc.PID,
		Ppid:          proc.PPID,
		User:          proc.User,
		Role:          roleToProto(proc.Role),
		CognitiveTier: cogToProto(proc.CognitiveTier),
		Config: &pb.AgentConfig{
			Name:         proc.Name,
			Model:        proc.Model,
			SystemPrompt: proc.SystemPrompt,
			Metadata:     proc.Metadata,
		},
		Limits: limitsToProto(proc.Limits),
	})
	if err != nil {
		conn.Close()
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("runtime PID %d: Init RPC failed: %w", proc.PID, err)
	}
	if !initResp.Ready {
		conn.Close()
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("runtime PID %d: Init not ready: %s", proc.PID, initResp.Error)
	}

	proc.RuntimeAddr = agentAddr
	hklog.For("runtime").Info("agent initialized", "pid", proc.PID, "name", proc.Name)

	rt := &AgentRuntime{
		PID:         proc.PID,
		Type:        rtType,
		Addr:        agentAddr,
		ProcessInfo: proc,
		Cmd:         cmd,
		Conn:        conn,
		Client:      client,
		exitCh:      make(chan struct{}),
	}

	// Exit watcher: detect when the OS process dies on its own.
	go m.watchProcessExit(rt)

	return rt, nil
}

// watchProcessExit waits for cmd.Wait() and handles cleanup if the process
// died on its own (not via StopRuntime).
func (m *Manager) watchProcessExit(rt *AgentRuntime) {
	err := rt.Cmd.Wait()
	exitCode := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	}
	close(rt.exitCh)

	// Try to remove from runtimes map. If already removed (by StopRuntime),
	// skip cleanup — StopRuntime handles it.
	m.mu.Lock()
	_, stillTracked := m.runtimes[rt.PID]
	if stillTracked {
		delete(m.runtimes, rt.PID)
	}
	m.mu.Unlock()

	if !stillTracked {
		return // StopRuntime already handled cleanup.
	}

	// Process died on its own — clean up and notify.
	hklog.For("runtime").Warn("process exited on its own", "pid", rt.PID, "name", rt.ProcessInfo.Name, "exit_code", exitCode)

	if rt.Conn != nil {
		rt.Conn.Close()
	}

	if m.onProcessExit != nil {
		m.onProcessExit(rt.PID, exitCode)
	}
}

// StopRuntime terminates an agent runtime.
// The exit watcher goroutine will NOT call onProcessExit because we remove
// the runtime from the map before the process exits.
func (m *Manager) StopRuntime(pid process.PID) error {
	m.mu.Lock()
	rt, ok := m.runtimes[pid]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("no runtime for PID %d", pid)
	}
	delete(m.runtimes, pid)
	m.mu.Unlock()

	hklog.For("runtime").Info("stopping runtime", "pid", pid, "name", rt.ProcessInfo.Name)

	// Virtual process — nothing to stop.
	if rt.Cmd == nil {
		return nil
	}

	// Send Shutdown RPC with 5s grace period.
	if rt.Client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = rt.Client.Shutdown(ctx, &pb.ShutdownRequest{
			Reason:              pb.ShutdownReason_SHUTDOWN_NORMAL,
			GracePeriodSeconds:  5,
		})
		cancel()
	}

	// Wait for process exit via exitCh (set by the exit watcher goroutine).
	select {
	case <-rt.exitCh:
		// Process exited gracefully.
	case <-time.After(5 * time.Second):
		hklog.For("runtime").Warn("process did not exit in time, killing", "pid", pid)
		_ = rt.Cmd.Process.Kill()
		select {
		case <-rt.exitCh:
			// Process exited after kill.
		case <-time.After(2 * time.Second):
			hklog.For("runtime").Error("kill did not take effect, abandoning", "pid", pid)
		}
	}

	// Close gRPC connection.
	if rt.Conn != nil {
		rt.Conn.Close()
	}

	return nil
}

// StopAll terminates all running agent runtimes.
// Used during graceful shutdown.
func (m *Manager) StopAll() {
	m.mu.RLock()
	pids := make([]process.PID, 0, len(m.runtimes))
	for pid := range m.runtimes {
		pids = append(pids, pid)
	}
	m.mu.RUnlock()

	for _, pid := range pids {
		if err := m.StopRuntime(pid); err != nil {
			hklog.For("runtime").Error("StopAll failed for process", "pid", pid, "error", err)
		}
	}
}

// KillAll forcefully terminates all running agent runtimes without graceful shutdown.
// Used when the user sends a second interrupt signal.
func (m *Manager) KillAll() {
	m.mu.Lock()
	rts := make([]*AgentRuntime, 0, len(m.runtimes))
	for _, rt := range m.runtimes {
		rts = append(rts, rt)
	}
	// Clear the map so exit watchers skip cleanup.
	for _, rt := range rts {
		delete(m.runtimes, rt.PID)
	}
	m.mu.Unlock()

	for _, rt := range rts {
		if rt.Cmd != nil && rt.Cmd.Process != nil {
			hklog.For("runtime").Warn("force-killing process", "pid", rt.PID, "name", rt.ProcessInfo.Name)
			_ = rt.Cmd.Process.Kill()
		}
		if rt.Conn != nil {
			rt.Conn.Close()
		}
	}
}

// GetRuntime returns the runtime for a PID, or nil.
func (m *Manager) GetRuntime(pid process.PID) *AgentRuntime {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.runtimes[pid]
}

// GetClient returns the AgentServiceClient for the given PID, or nil.
func (m *Manager) GetClient(pid process.PID) pb.AgentServiceClient {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rt := m.runtimes[pid]
	if rt == nil {
		return nil
	}
	return rt.Client
}

// ListRuntimes returns all active runtimes.
func (m *Manager) ListRuntimes() []*AgentRuntime {
	m.mu.RLock()
	defer m.mu.RUnlock()

	list := make([]*AgentRuntime, 0, len(m.runtimes))
	for _, rt := range m.runtimes {
		list = append(list, rt)
	}
	return list
}

// cogToProto converts internal CognitiveTier to proto enum.
func cogToProto(c process.CognitiveTier) pb.CognitiveTier {
	switch c {
	case process.CogStrategic:
		return pb.CognitiveTier_COG_STRATEGIC
	case process.CogTactical:
		return pb.CognitiveTier_COG_TACTICAL
	case process.CogOperational:
		return pb.CognitiveTier_COG_OPERATIONAL
	default:
		return pb.CognitiveTier_COG_OPERATIONAL
	}
}

// limitsToProto converts internal ResourceLimits to proto message.
func limitsToProto(l process.ResourceLimits) *pb.ResourceLimits {
	return &pb.ResourceLimits{
		MaxTokensPerHour:   l.MaxTokensPerHour,
		MaxContextTokens:   l.MaxContextTokens,
		MaxChildren:        l.MaxChildren,
		MaxConcurrentTasks: l.MaxConcurrentTasks,
		TimeoutSeconds:     l.TimeoutSeconds,
		MaxTokensTotal:     l.MaxTokensTotal,
	}
}

// roleToProto converts internal AgentRole to proto enum.
func roleToProto(r process.AgentRole) pb.AgentRole {
	switch r {
	case process.RoleKernel:
		return pb.AgentRole_ROLE_KERNEL
	case process.RoleDaemon:
		return pb.AgentRole_ROLE_DAEMON
	case process.RoleAgent:
		return pb.AgentRole_ROLE_AGENT
	case process.RoleArchitect:
		return pb.AgentRole_ROLE_ARCHITECT
	case process.RoleLead:
		return pb.AgentRole_ROLE_LEAD
	case process.RoleWorker:
		return pb.AgentRole_ROLE_WORKER
	case process.RoleTask:
		return pb.AgentRole_ROLE_TASK
	default:
		return pb.AgentRole_ROLE_TASK
	}
}
