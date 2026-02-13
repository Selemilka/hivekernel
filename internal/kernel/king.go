package kernel

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/selemilka/hivekernel/internal/cluster"
	"github.com/selemilka/hivekernel/internal/ipc"
	"github.com/selemilka/hivekernel/internal/permissions"
	"github.com/selemilka/hivekernel/internal/process"
	"github.com/selemilka/hivekernel/internal/resources"
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

	proc *process.Process // king's own process entry

	mu     sync.RWMutex
	cancel context.CancelFunc
}

// New creates a new King instance and bootstraps PID 1.
func New(cfg Config) (*King, error) {
	registry := process.NewRegistry()
	spawner := process.NewSpawner(registry)

	// Bootstrap PID 1.
	kernelProc, err := spawner.SpawnKernel("king", cfg.KernelUser, cfg.NodeName)
	if err != nil {
		return nil, fmt.Errorf("bootstrap king: %w", err)
	}

	auth := permissions.NewAuthProvider(registry)

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
		proc:        kernelProc,
	}

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

	log.Printf("[king] bootstrapped as PID %d on %s", kernelProc.PID, cfg.NodeName)
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

// PID returns the kernel's process ID.
func (k *King) PID() process.PID {
	return k.proc.PID
}

// Run starts the kernel main loop. Blocks until ctx is cancelled.
func (k *King) Run(ctx context.Context) error {
	ctx, k.cancel = context.WithCancel(ctx)
	log.Printf("[king] running, listening for messages...")

	// Main loop: process incoming messages.
	for {
		msg := k.inbox.PopWait(ctx.Done())
		if msg == nil {
			log.Printf("[king] shutting down")
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

	log.Printf("[king] spawned %s (PID %d) under PID %d, role=%s, cog=%s",
		proc.Name, proc.PID, proc.PPID, proc.Role, proc.CognitiveTier)
	return proc, nil
}

// handleMessage dispatches a message to the appropriate handler.
func (k *King) handleMessage(msg *ipc.Message) {
	log.Printf("[king] received message %s from PID %d, type=%s, priority=%d",
		msg.ID, msg.FromPID, msg.Type, msg.Priority)

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
	log.Printf("[king] escalation from PID %d: %s", msg.FromPID, string(msg.Payload))
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
		log.Printf("[king] no migration target available: %v", err)
		return
	}

	mig, err := k.migrator.PrepareMigration(fromPID, target.ID)
	if err != nil {
		log.Printf("[king] migration preparation failed: %v", err)
		return
	}

	if err := k.migrator.ExecuteMigration(mig.ID); err != nil {
		log.Printf("[king] migration execution failed: %v", err)
		return
	}

	log.Printf("[king] migrated PID %d branch from %s to %s", fromPID, sourceNode, target.ID)
	k.eventBus.Publish("migration_completed", fromPID,
		[]byte(fmt.Sprintf("migrated from %s to %s", sourceNode, target.ID)))
}

func (k *King) handleSpawnRequest(msg *ipc.Message) {
	log.Printf("[king] spawn request from PID %d: %s", msg.FromPID, string(msg.Payload))
}

func (k *King) routeMessage(msg *ipc.Message) {
	if msg.ToPID == 0 && msg.ToQueue == "" {
		log.Printf("[king] message %s has no target, dropping", msg.ID)
		return
	}
	// Route through broker (validates rules, computes priority, delivers).
	if err := k.broker.Route(msg); err != nil {
		log.Printf("[king] routing failed for message %s: %v", msg.ID, err)
	}
}

// PrintProcessTable logs the current process table (ps-like output).
func (k *King) PrintProcessTable() {
	procs := k.registry.List()
	log.Printf("PID  PPID USER       ROLE       COG        MODEL   VPS   STATE    COMMAND")
	log.Printf("-------------------------------------------------------------------")
	for _, p := range procs {
		log.Printf("%-4d %-4d %-10s %-10s %-10s %-7s %-5s %-8s %s",
			p.PID, p.PPID, p.User, p.Role, p.CognitiveTier,
			p.Model, p.VPS, p.State, p.Name)
	}
}
