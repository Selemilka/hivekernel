package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	pb "github.com/selemilka/hivekernel/api/proto/hivepb"
	"github.com/selemilka/hivekernel/internal/cluster"
	"github.com/selemilka/hivekernel/internal/hklog"
	"github.com/selemilka/hivekernel/internal/ipc"
	"github.com/selemilka/hivekernel/internal/permissions"
	"github.com/selemilka/hivekernel/internal/process"
	"github.com/selemilka/hivekernel/internal/resources"
	"github.com/selemilka/hivekernel/internal/runtime"
	"github.com/selemilka/hivekernel/internal/scheduler"
)

// King is the root process (PID 1) of HiveKernel.
// It owns the process registry, spawner, broker, shared memory, and event bus.
type King struct {
	config   Config
	registry *process.Registry
	spawner  *process.Spawner
	inbox    *ipc.PriorityQueue

	// Phase 2: IPC subsystems
	broker   *ipc.Broker
	sharedMem *ipc.SharedMemory
	eventBus *ipc.EventBus
	pipes    *ipc.PipeRegistry

	// Phase 3: Resources + Permissions
	budget      *resources.BudgetManager
	rateLimiter *resources.RateLimiter
	limits      *resources.LimitChecker
	accountant  *resources.Accountant
	auth        *permissions.AuthProvider
	acl         *permissions.ACL
	caps        *permissions.CapabilityChecker

	// Phase 4: Multi-VPS
	nodes     *cluster.NodeRegistry
	connector *cluster.Connector
	migrator  *cluster.MigrationManager
	cgroups   *resources.CGroupManager

	// Phase 5: Dynamic Scaling
	cron      *scheduler.CronScheduler
	lifecycle *process.LifecycleManager
	signals   *process.SignalRouter

	// Phase 7: Runtime manager
	rtManager *runtime.Manager

	// Event sourcing
	eventLog *process.EventLog

	// Distributed tracing
	traceMu    sync.RWMutex
	traceStore map[process.PID]*traceCtx

	// Task execution (set after creation via SetExecutor).
	executor *runtime.Executor

	proc *process.Process // king's own process entry

	mu     sync.RWMutex
	cancel context.CancelFunc
}

// traceCtx holds trace context for a single process.
type traceCtx struct {
	TraceID   string
	TraceSpan string
}

// New creates a new King instance and bootstraps PID 1.
func New(cfg Config) (*King, error) {
	registry := process.NewRegistry()
	spawner := process.NewSpawner(registry)

	// Create event log directory and event log.
	logDir := filepath.Join("logs", "events")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	logPath := filepath.Join(logDir, fmt.Sprintf("%s.jsonl", time.Now().Format("20060102-150405")))
	eventLog, err := process.NewEventLog(4096, logPath)
	if err != nil {
		return nil, fmt.Errorf("create event log: %w", err)
	}
	registry.SetEventLog(eventLog)

	// Bootstrap PID 1.
	kernelProc, err := spawner.SpawnKernel("king", cfg.KernelUser, cfg.NodeName)
	if err != nil {
		return nil, fmt.Errorf("bootstrap king: %w", err)
	}

	auth := permissions.NewAuthProvider(registry)
	signals := process.NewSignalRouter(registry)

	nodes := cluster.NewNodeRegistry()

	k := &King{
		config:      cfg,
		registry:    registry,
		spawner:     spawner,
		inbox:       ipc.NewPriorityQueue(cfg.MessageAgingFactor),
		broker:      ipc.NewBroker(registry, cfg.MessageAgingFactor),
		sharedMem:   ipc.NewSharedMemory(registry),
		eventBus:    ipc.NewEventBus(),
		pipes:       ipc.NewPipeRegistry(),
		budget:      resources.NewBudgetManager(registry),
		rateLimiter: resources.NewRateLimiter(),
		limits:      resources.NewLimitChecker(registry),
		accountant:  resources.NewAccountant(registry),
		auth:        auth,
		acl:         permissions.NewACL(registry, auth),
		caps:        permissions.NewCapabilityChecker(registry),
		nodes:       nodes,
		connector:   cluster.NewConnector(),
		migrator:    cluster.NewMigrationManager(registry, nodes),
		cgroups:     resources.NewCGroupManager(registry),
		cron:        scheduler.NewCronScheduler(),
		lifecycle:   process.NewLifecycleManager(registry, signals),
		signals:     signals,
		proc:        kernelProc,
		eventLog:    eventLog,
		traceStore:  make(map[process.PID]*traceCtx),
	}

	// Wire trace lookup to EventLog for auto-annotation.
	eventLog.SetTraceLookup(func(pid process.PID) (string, string) {
		return k.GetTrace(pid)
	})

	// Register this node in the cluster registry.
	nodes.Register(&cluster.NodeInfo{
		ID:       cfg.NodeName,
		Address:  cfg.ListenAddr,
		Cores:    2,
		MemoryMB: 2048,
	})

	// Set kernel's initial budget (large defaults).
	k.budget.SetBudget(kernelProc.PID, resources.TierOpus, cfg.DefaultLimits.MaxTokensTotal)
	k.budget.SetBudget(kernelProc.PID, resources.TierSonnet, cfg.DefaultLimits.MaxTokensTotal*10)
	k.budget.SetBudget(kernelProc.PID, resources.TierMini, cfg.DefaultLimits.MaxTokensTotal*100)

	hklog.For("king").Info("bootstrapped", "pid", kernelProc.PID, "node", cfg.NodeName)
	return k, nil
}

// Registry returns the process registry.
func (k *King) Registry() *process.Registry {
	return k.registry
}

// Spawner returns the process spawner.
func (k *King) Spawner() *process.Spawner {
	return k.spawner
}

// Inbox returns the kernel's message queue.
func (k *King) Inbox() *ipc.PriorityQueue {
	return k.inbox
}

// Broker returns the message broker.
func (k *King) Broker() *ipc.Broker {
	return k.broker
}

// SharedMemory returns the shared memory store.
func (k *King) SharedMemory() *ipc.SharedMemory {
	return k.sharedMem
}

// EventBus returns the event bus.
func (k *King) EventBus() *ipc.EventBus {
	return k.eventBus
}

// Pipes returns the pipe registry.
func (k *King) Pipes() *ipc.PipeRegistry {
	return k.pipes
}

// Budget returns the budget manager.
func (k *King) Budget() *resources.BudgetManager {
	return k.budget
}

// RateLimiter returns the rate limiter.
func (k *King) RateLimiter() *resources.RateLimiter {
	return k.rateLimiter
}

// Limits returns the limit checker.
func (k *King) Limits() *resources.LimitChecker {
	return k.limits
}

// Accountant returns the usage accountant.
func (k *King) Accountant() *resources.Accountant {
	return k.accountant
}

// Auth returns the auth provider.
func (k *King) Auth() *permissions.AuthProvider {
	return k.auth
}

// ACL returns the access control list.
func (k *King) ACL() *permissions.ACL {
	return k.acl
}

// Caps returns the capability checker.
func (k *King) Caps() *permissions.CapabilityChecker {
	return k.caps
}

// Nodes returns the cluster node registry.
func (k *King) Nodes() *cluster.NodeRegistry {
	return k.nodes
}

// Connector returns the VPS connector.
func (k *King) Connector() *cluster.Connector {
	return k.connector
}

// Migrator returns the migration manager.
func (k *King) Migrator() *cluster.MigrationManager {
	return k.migrator
}

// CGroups returns the cgroup manager.
func (k *King) CGroups() *resources.CGroupManager {
	return k.cgroups
}

// Cron returns the cron scheduler.
func (k *King) Cron() *scheduler.CronScheduler {
	return k.cron
}

// Lifecycle returns the lifecycle manager.
func (k *King) Lifecycle() *process.LifecycleManager {
	return k.lifecycle
}

// Signals returns the signal router.
func (k *King) Signals() *process.SignalRouter {
	return k.signals
}

// SetRuntimeManager sets the runtime manager (called during wiring).
func (k *King) SetRuntimeManager(m *runtime.Manager) {
	k.rtManager = m
}

// RuntimeManager returns the runtime manager.
func (k *King) RuntimeManager() *runtime.Manager {
	return k.rtManager
}

// EventLog returns the process event log.
func (k *King) EventLog() *process.EventLog {
	return k.eventLog
}

// SetExecutor sets the executor for cron task execution (called during wiring).
func (k *King) SetExecutor(e *runtime.Executor) {
	k.executor = e
}

// PID returns the kernel's process ID.
func (k *King) PID() process.PID {
	return k.proc.PID
}

// ResolveAgent returns the agent name for a PID.
// Implements tracing.PIDResolver.
func (k *King) ResolveAgent(pid uint64) (name string, ok bool) {
	p, err := k.registry.Get(pid)
	if err != nil {
		return "", false
	}
	return p.Name, true
}

// Run starts the kernel main loop. Blocks until ctx is cancelled.
func (k *King) Run(ctx context.Context) error {
	ctx, k.cancel = context.WithCancel(ctx)
	hklog.For("king").Info("running, listening for messages")

	// Main loop: process incoming messages.
	for {
		msg := k.inbox.PopWait(ctx.Done())
		if msg == nil {
			hklog.For("king").Info("shutting down")
			return ctx.Err()
		}
		k.handleMessage(msg)
	}
}

// Stop gracefully stops the kernel.
func (k *King) Stop() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cancel != nil {
		k.cancel()
	}
}

// SetTrace stores trace context for a process.
func (k *King) SetTrace(pid process.PID, traceID, traceSpan string) {
	k.traceMu.Lock()
	defer k.traceMu.Unlock()
	k.traceStore[pid] = &traceCtx{TraceID: traceID, TraceSpan: traceSpan}
}

// GetTrace returns trace context for a process.
func (k *King) GetTrace(pid process.PID) (traceID, traceSpan string) {
	k.traceMu.RLock()
	defer k.traceMu.RUnlock()
	if tc := k.traceStore[pid]; tc != nil {
		return tc.TraceID, tc.TraceSpan
	}
	return "", ""
}

// ClearTrace removes trace context for a process.
func (k *King) ClearTrace(pid process.PID) {
	k.traceMu.Lock()
	defer k.traceMu.Unlock()
	delete(k.traceStore, pid)
}

// SpawnChild creates a child process under the given parent.
// Phase 3: validates ACL, capabilities, auth, and budget before spawning.
func (k *King) SpawnChild(req process.SpawnRequest) (*process.Process, error) {
	// Check ACL: does the parent have permission to spawn?
	if err := k.acl.Check(req.ParentPID, permissions.ActionSpawn); err != nil {
		return nil, fmt.Errorf("spawn denied: %w", err)
	}

	// Check capability: does the parent's role allow spawning?
	if err := k.caps.RequireCapability(req.ParentPID, permissions.CapSpawnChildren); err != nil {
		return nil, fmt.Errorf("spawn denied: %w", err)
	}

	// Validate user inheritance.
	if err := k.auth.ValidateInheritance(req.ParentPID, req.User); err != nil {
		return nil, fmt.Errorf("spawn denied: %w", err)
	}

	// Validate tool capabilities for the child's role.
	if len(req.Tools) > 0 {
		// Create a temporary process to check against the child's role.
		childRole := req.Role
		// Check tools against what the child role permits.
		tempReg := process.NewRegistry()
		tempReg.Register(&process.Process{Role: childRole})
		tempCaps := permissions.NewCapabilityChecker(tempReg)
		if err := tempCaps.ValidateTools(1, req.Tools); err != nil {
			return nil, fmt.Errorf("spawn denied: child role %s cannot use requested tools: %w", childRole, err)
		}
	}

	// Phase 4: Check cgroup limits (if parent is in a group).
	if err := k.cgroups.CheckSpawnAllowed(req.ParentPID); err != nil {
		return nil, fmt.Errorf("spawn denied: %w", err)
	}

	// Spawn the process (validates cognitive tier, max_children, etc.).
	proc, err := k.spawner.Spawn(req)
	if err != nil {
		return nil, err
	}

	// Allocate budget from parent to child (if child has token limits).
	childTier := resources.TierFromCog(req.CognitiveTier)
	if req.Limits.MaxTokensTotal > 0 {
		// Ensure parent has a budget entry at this tier.
		if parentBudget := k.budget.GetBudget(req.ParentPID, childTier); parentBudget != nil {
			if err := k.budget.Allocate(req.ParentPID, proc.PID, childTier, req.Limits.MaxTokensTotal); err != nil {
				// Rollback: remove the spawned process.
				k.registry.Remove(proc.PID)
				return nil, fmt.Errorf("spawn denied: %w", err)
			}
		} else {
			// Parent has no budget at this tier — just set child's budget directly.
			k.budget.SetBudget(proc.PID, childTier, req.Limits.MaxTokensTotal)
		}
	}

	// Phase 4: If parent is in a cgroup, add the child to the same group.
	if g, ok := k.cgroups.GetByPID(req.ParentPID); ok {
		_ = k.cgroups.AddProcess(g.Name, proc.PID)
	}

	// Phase 7: Start runtime if agent code is specified.
	if req.RuntimeImage != "" && k.rtManager != nil {
		proc.RuntimeAddr = req.RuntimeImage // Pass image spec through RuntimeAddr.
		if _, err := k.rtManager.StartRuntime(proc, runtime.RuntimeType(req.RuntimeType)); err != nil {
			k.registry.Remove(proc.PID)
			return nil, fmt.Errorf("runtime start failed: %w", err)
		}
	}

	// Propagate trace context from parent to child.
	if tID, tSpan := k.GetTrace(req.ParentPID); tID != "" {
		childSpan := tSpan + "/" + fmt.Sprint(proc.PID)
		k.SetTrace(proc.PID, tID, childSpan)
	}

	hklog.For("king").Info("spawned child", "name", proc.Name, "pid", proc.PID, "ppid", proc.PPID, "role", proc.Role, "cog", proc.CognitiveTier)
	return proc, nil
}

// handleMessage dispatches a message to the appropriate handler.
func (k *King) handleMessage(msg *ipc.Message) {
	hklog.For("king").Debug("received message", "id", msg.ID, "from_pid", msg.FromPID, "type", msg.Type, "priority", msg.Priority)

	switch msg.Type {
	case "escalation":
		k.handleEscalation(msg)
	case "spawn_request":
		k.handleSpawnRequest(msg)
	default:
		k.routeMessage(msg)
	}
}

func (k *King) handleEscalation(msg *ipc.Message) {
	hklog.For("king").Warn("escalation received", "from_pid", msg.FromPID, "payload", string(msg.Payload))
	// Publish as event so subscribers can react.
	k.eventBus.Publish("escalation", msg.FromPID, msg.Payload)

	// Phase 4: Check if escalation is about an overloaded VPS.
	payload := string(msg.Payload)
	if payload == "vps_overloaded" {
		k.handleOverloadEscalation(msg.FromPID)
	}
}

// handleOverloadEscalation attempts to migrate processes from an overloaded VPS.
func (k *King) handleOverloadEscalation(fromPID process.PID) {
	proc, err := k.registry.Get(fromPID)
	if err != nil {
		return
	}

	sourceNode := proc.VPS
	target, err := k.nodes.FindLeastLoaded(sourceNode)
	if err != nil {
		hklog.For("king").Warn("no migration target available", "error", err)
		return
	}

	mig, err := k.migrator.PrepareMigration(fromPID, target.ID)
	if err != nil {
		hklog.For("king").Error("migration preparation failed", "error", err)
		return
	}

	if err := k.migrator.ExecuteMigration(mig.ID); err != nil {
		hklog.For("king").Error("migration execution failed", "error", err)
		return
	}

	hklog.For("king").Info("migrated branch", "pid", fromPID, "from", sourceNode, "to", target.ID)
	k.eventBus.Publish("migration_completed", fromPID,
		[]byte(fmt.Sprintf("migrated from %s to %s", sourceNode, target.ID)))
}

func (k *King) handleSpawnRequest(msg *ipc.Message) {
	hklog.For("king").Info("spawn request", "from_pid", msg.FromPID, "payload", string(msg.Payload))
}

func (k *King) routeMessage(msg *ipc.Message) {
	if msg.ToPID == 0 && msg.ToQueue == "" {
		hklog.For("king").Warn("message has no target, dropping", "id", msg.ID)
		return
	}
	// Route through broker (validates rules, computes priority, delivers).
	if err := k.broker.Route(msg); err != nil {
		hklog.For("king").Error("routing failed", "id", msg.ID, "error", err)
	}
}

// RunCronPoller ticks every second, checks for due cron entries,
// and executes them. Blocks until ctx is cancelled.
func (k *King) RunCronPoller(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	hklog.For("cron").Info("poller started", "interval", "1s")
	for {
		select {
		case <-ctx.Done():
			hklog.For("cron").Info("poller stopped")
			return
		case <-ticker.C:
			due := k.cron.CheckDue(time.Now())
			for _, entry := range due {
				go k.executeCronEntry(ctx, entry)
			}
		}
	}
}

// executeCronEntry dispatches a single cron entry based on its action type.
func (k *King) executeCronEntry(ctx context.Context, entry *scheduler.CronEntry) {
	cronLog := hklog.For("cron")
	cronLog.Info("firing cron entry", "name", entry.Name, "action", entry.Action, "target_pid", entry.TargetPID)

	switch entry.Action {
	case scheduler.CronExecute:
		if k.executor == nil || k.rtManager == nil {
			cronLog.Error("executor or runtime manager not available", "name", entry.Name)
			return
		}
		rt := k.rtManager.GetRuntime(entry.TargetPID)
		if rt == nil {
			cronLog.Error("no runtime for target", "name", entry.Name, "pid", entry.TargetPID)
			return
		}
		cronTraceID := fmt.Sprintf("cron-%s-%d", entry.ID, time.Now().Unix())
		params := make(map[string]string)
		for k, v := range entry.ExecuteParams {
			params[k] = v
		}
		params["trace_id"] = cronTraceID
		taskReq := &pb.TaskRequest{
			TaskId:      cronTraceID,
			Description: entry.ExecuteDesc,
			Params:      params,
		}
		// Set trace for the target PID so all events get annotated.
		k.SetTrace(entry.TargetPID, cronTraceID, "cron")
		// Emit message_sent event so dashboard shows the cron execution.
		if el := k.eventLog; el != nil {
			toName := ""
			if receiver, err := k.registry.Get(entry.TargetPID); err == nil {
				toName = receiver.Name
			}
			payload := map[string]interface{}{
				"description": entry.ExecuteDesc,
				"params":      entry.ExecuteParams,
			}
			jsonPayload, _ := json.Marshal(payload)
			el.Emit(process.ProcessEvent{
				Type:           process.EventMessageSent,
				PID:            1,
				PPID:           entry.TargetPID,
				Name:           "king",
				Role:           toName,
				Message:        "cron_execute",
				PayloadPreview: truncatePayload(string(jsonPayload), 2000),
			})
		}
		execStart := time.Now()
		result, err := k.executor.ExecuteTask(ctx, rt.Addr, entry.TargetPID, taskReq)
		durationMs := time.Since(execStart).Milliseconds()
		if err != nil {
			cronLog.Error("execute failed", "name", entry.Name, "error", err)
			entry.LastExitCode = 1
			entry.LastOutput = fmt.Sprintf("error: %v", err)
			entry.LastDurationMs = durationMs
			if el := k.eventLog; el != nil {
				el.Emit(process.ProcessEvent{
					Type:    process.EventCronExecuted,
					PID:     entry.TargetPID,
					Name:    entry.Name,
					Level:   "error",
					Message: fmt.Sprintf("[cron] %q: execute failed: %v", entry.Name, err),
					Fields: map[string]string{
						"cron_id":     entry.ID,
						"cron_name":   entry.Name,
						"exit_code":   "1",
						"duration_ms": fmt.Sprint(durationMs),
					},
				})
			}
			return
		}
		entry.LastExitCode = result.ExitCode
		entry.LastOutput = truncatePayload(result.Output, 4000)
		entry.LastDurationMs = durationMs
		cronLog.Info("cron completed", "name", entry.Name, "exit_code", result.ExitCode, "duration_ms", durationMs, "output_bytes", len(result.Output))
		if el := k.eventLog; el != nil {
			level := "info"
			if result.ExitCode != 0 {
				level = "warn"
			}
			el.Emit(process.ProcessEvent{
				Type:    process.EventCronExecuted,
				PID:     entry.TargetPID,
				Name:    entry.Name,
				Level:   level,
				Message: fmt.Sprintf("[cron] %q: exit=%d, output=%s", entry.Name, result.ExitCode, truncatePayload(result.Output, 500)),
				Fields: map[string]string{
					"cron_id":        entry.ID,
					"cron_name":      entry.Name,
					"exit_code":      fmt.Sprint(result.ExitCode),
					"output_preview": truncatePayload(result.Output, 500),
					"duration_ms":    fmt.Sprint(durationMs),
				},
			})
		}

	case scheduler.CronMessage:
		payload := map[string]interface{}{
			"description": entry.ExecuteDesc,
			"params":      entry.ExecuteParams,
		}
		jsonPayload, err := json.Marshal(payload)
		if err != nil {
			cronLog.Error("failed to marshal payload", "name", entry.Name, "error", err)
			return
		}
		msg := &ipc.Message{
			FromPID:  1, // King
			FromName: "king",
			ToPID:    entry.TargetPID,
			Type:     "cron_task",
			Payload:  jsonPayload,
		}
		if err := k.broker.Route(msg); err != nil {
			cronLog.Error("message route failed", "name", entry.Name, "error", err)
			return
		}
		// Emit message_sent event for dashboard visibility.
		if el := k.eventLog; el != nil {
			toName := ""
			if receiver, err := k.registry.Get(entry.TargetPID); err == nil {
				toName = receiver.Name
			}
			el.Emit(process.ProcessEvent{
				Type:           process.EventMessageSent,
				PID:            1,
				PPID:           entry.TargetPID,
				Name:           "king",
				Role:           toName,
				Message:        "cron_task",
				PayloadPreview: truncatePayload(string(jsonPayload), 2000),
			})
		}
		cronLog.Info("message sent", "name", entry.Name, "target_pid", entry.TargetPID)

	case scheduler.CronSpawn:
		_, err := k.SpawnChild(process.SpawnRequest{
			ParentPID:     entry.SpawnParent,
			Name:          entry.SpawnName,
			Role:          entry.SpawnRole,
			CognitiveTier: entry.SpawnTier,
		})
		if err != nil {
			cronLog.Error("spawn failed", "name", entry.Name, "error", err)
		}

	case scheduler.CronWake:
		if err := k.registry.SetState(entry.TargetPID, process.StateRunning); err != nil {
			cronLog.Error("wake failed", "name", entry.Name, "error", err)
		}

	default:
		cronLog.Warn("unknown cron action", "name", entry.Name, "action", entry.Action)
	}
}

// EmitLog implements hklog.LogEmitter, forwarding kernel slog records to the EventLog.
func (k *King) EmitLog(pid uint64, level, component, message string, fields map[string]string) {
	el := k.eventLog
	if el == nil {
		return
	}
	if fields == nil {
		fields = map[string]string{}
	}
	fields["component"] = component
	el.Emit(process.ProcessEvent{
		Type:    process.EventLogged,
		PID:     process.PID(pid),
		Name:    component,
		Level:   level,
		Message: message,
		Fields:  fields,
	})
}

// PrintProcessTable logs the current process table (ps-like output).
func (k *King) PrintProcessTable() {
	procs := k.registry.List()
	fmt.Printf("PID  PPID USER       ROLE       COG        MODEL   VPS   STATE    COMMAND\n")
	fmt.Printf("-------------------------------------------------------------------\n")
	for _, p := range procs {
		fmt.Printf("%-4d %-4d %-10s %-10s %-10s %-7s %-5s %-8s %s\n",
			p.PID, p.PPID, p.User, p.Role, p.CognitiveTier,
			p.Model, p.VPS, p.State, p.Name)
	}
}
