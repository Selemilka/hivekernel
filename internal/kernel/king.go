package kernel

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/selemilka/hivekernel/internal/ipc"
	"github.com/selemilka/hivekernel/internal/process"
)

// King is the root process (PID 1) of HiveKernel.
// It owns the process registry, spawner, and message queue.
type King struct {
	config   Config
	registry *process.Registry
	spawner  *process.Spawner
	inbox    *ipc.PriorityQueue

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

	k := &King{
		config:   cfg,
		registry: registry,
		spawner:  spawner,
		inbox:    ipc.NewPriorityQueue(cfg.MessageAgingFactor),
		proc:     kernelProc,
	}

	log.Printf("[king] bootstrapped as PID %d on %s", kernelProc.PID, cfg.NodeName)
	return k, nil
}

// Registry returns the process registry (used by gRPC service implementations).
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
			// Context cancelled.
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
func (k *King) SpawnChild(req process.SpawnRequest) (*process.Process, error) {
	proc, err := k.spawner.Spawn(req)
	if err != nil {
		return nil, err
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
		// Route to target process.
		k.routeMessage(msg)
	}
}

func (k *King) handleEscalation(msg *ipc.Message) {
	log.Printf("[king] escalation from PID %d: %s", msg.FromPID, string(msg.Payload))
	// Phase 0: just log it. Later phases will add decision-making.
}

func (k *King) handleSpawnRequest(msg *ipc.Message) {
	log.Printf("[king] spawn request from PID %d: %s", msg.FromPID, string(msg.Payload))
	// Phase 0: spawn requests go through SpawnChild directly via gRPC.
}

func (k *King) routeMessage(msg *ipc.Message) {
	if msg.ToPID == 0 {
		log.Printf("[king] message %s has no target, dropping", msg.ID)
		return
	}
	// Phase 0: log routing. Full broker in Phase 2.
	log.Printf("[king] routing message %s to PID %d", msg.ID, msg.ToPID)
}

// PrintProcessTable logs the current process table (ps-like output).
func (k *King) PrintProcessTable() {
	procs := k.registry.List()
	log.Printf("PID  PPID USER       ROLE       COG        MODEL   VPS   STATE    COMMAND")
	log.Printf("─────────────────────────────────────────────────────────────────────────")
	for _, p := range procs {
		log.Printf("%-4d %-4d %-10s %-10s %-10s %-7s %-5s %-8s %s",
			p.PID, p.PPID, p.User, p.Role, p.CognitiveTier,
			p.Model, p.VPS, p.State, p.Name)
	}
}
