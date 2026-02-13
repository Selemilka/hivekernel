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
		config:    cfg,
		registry:  registry,
		spawner:   spawner,
		inbox:     ipc.NewPriorityQueue(cfg.MessageAgingFactor),
		broker:    ipc.NewBroker(registry, cfg.MessageAgingFactor),
		sharedMem: ipc.NewSharedMemory(registry),
		eventBus:  ipc.NewEventBus(),
		pipes:     ipc.NewPipeRegistry(),
		proc:      kernelProc,
	}

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
		k.routeMessage(msg)
	}
}

func (k *King) handleEscalation(msg *ipc.Message) {
	log.Printf("[king] escalation from PID %d: %s", msg.FromPID, string(msg.Payload))
	// Publish as event so subscribers can react.
	k.eventBus.Publish("escalation", msg.FromPID, msg.Payload)
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
