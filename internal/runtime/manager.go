package runtime

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	pb "github.com/selemilka/hivekernel/api/proto/hivepb"
	"github.com/selemilka/hivekernel/internal/process"

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
}

// Manager manages agent runtime processes (OS process spawn + gRPC connection).
type Manager struct {
	mu        sync.RWMutex
	runtimes  map[process.PID]*AgentRuntime
	coreAddr  string // core's gRPC address (agents connect back to this)
	pythonBin string // path to python executable
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
		log.Printf("[runtime] registered virtual runtime for PID %d (%s)", proc.PID, proc.Name)
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

// spawnReal launches the Python runner process, waits for READY, connects gRPC, calls Init.
func (m *Manager) spawnReal(proc *process.Process, rtType RuntimeType) (*AgentRuntime, error) {
	runtimeImage := proc.RuntimeAddr // abused as runtime image spec before real addr is set
	if runtimeImage == "" {
		return nil, fmt.Errorf("no RuntimeImage for PID %d", proc.PID)
	}

	// Build command: python -m hivekernel_sdk.runner --agent <image> --core <addr>
	cmd := exec.Command(
		m.pythonBin,
		"-m", "hivekernel_sdk.runner",
		"--agent", runtimeImage,
		"--core", m.coreAddr,
	)
	cmd.Env = append(cmd.Environ(),
		fmt.Sprintf("HIVEKERNEL_PID=%d", proc.PID),
		fmt.Sprintf("HIVEKERNEL_CORE=%s", m.coreAddr),
	)

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
	log.Printf("[runtime] PID %d agent listening on %s", proc.PID, agentAddr)

	// Dial gRPC to the agent.
	conn, err := grpc.NewClient(agentAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("runtime PID %d: dial agent: %w", proc.PID, err)
	}

	client := pb.NewAgentServiceClient(conn)

	// Call Init.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	initResp, err := client.Init(ctx, &pb.InitRequest{
		Pid:  proc.PID,
		Ppid: proc.PPID,
		User: proc.User,
		Role: roleToProto(proc.Role),
		Config: &pb.AgentConfig{
			Name: proc.Name,
		},
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
	log.Printf("[runtime] PID %d (%s) initialized successfully", proc.PID, proc.Name)

	return &AgentRuntime{
		PID:         proc.PID,
		Type:        rtType,
		Addr:        agentAddr,
		ProcessInfo: proc,
		Cmd:         cmd,
		Conn:        conn,
		Client:      client,
	}, nil
}

// StopRuntime terminates an agent runtime.
func (m *Manager) StopRuntime(pid process.PID) error {
	m.mu.Lock()
	rt, ok := m.runtimes[pid]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("no runtime for PID %d", pid)
	}
	delete(m.runtimes, pid)
	m.mu.Unlock()

	log.Printf("[runtime] stopping runtime for PID %d (%s)", pid, rt.ProcessInfo.Name)

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

	// Wait for process exit (5s timeout).
	done := make(chan error, 1)
	go func() { done <- rt.Cmd.Wait() }()

	select {
	case <-done:
		// Process exited gracefully.
	case <-time.After(5 * time.Second):
		log.Printf("[runtime] PID %d did not exit in time, killing", pid)
		_ = rt.Cmd.Process.Kill()
		<-done // Reap.
	}

	// Close gRPC connection.
	if rt.Conn != nil {
		rt.Conn.Close()
	}

	return nil
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
