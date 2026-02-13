package process

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// Signal represents a process signal (modeled after POSIX signals).
type Signal int

const (
	SIGTERM  Signal = iota // Graceful shutdown request
	SIGKILL                // Forced immediate termination
	SIGCHLD                // Child process exited
	SIGSTOP                // Pause execution (enter sleeping state)
	SIGCONT                // Resume execution
	SIGHUP                 // Configuration reload / parent changed
)

func (s Signal) String() string {
	switch s {
	case SIGTERM:
		return "SIGTERM"
	case SIGKILL:
		return "SIGKILL"
	case SIGCHLD:
		return "SIGCHLD"
	case SIGSTOP:
		return "SIGSTOP"
	case SIGCONT:
		return "SIGCONT"
	case SIGHUP:
		return "SIGHUP"
	default:
		return fmt.Sprintf("SIG(%d)", int(s))
	}
}

// ExitInfo contains information about a process exit (attached to SIGCHLD).
type ExitInfo struct {
	PID      PID
	ExitCode int
	Output   string
	ExitedAt time.Time
}

// SignalHandler is a callback invoked when a signal is delivered to a process.
type SignalHandler func(pid PID, sig Signal, info *ExitInfo)

// SignalRouter delivers signals to processes and manages signal handlers.
type SignalRouter struct {
	mu       sync.RWMutex
	handlers map[PID][]SignalHandler
	registry *Registry
}

// NewSignalRouter creates a new signal router backed by the given registry.
func NewSignalRouter(registry *Registry) *SignalRouter {
	return &SignalRouter{
		handlers: make(map[PID][]SignalHandler),
		registry: registry,
	}
}

// Register adds a signal handler for a process.
func (r *SignalRouter) Register(pid PID, handler SignalHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[pid] = append(r.handlers[pid], handler)
}

// Unregister removes all handlers for a process.
func (r *SignalRouter) Unregister(pid PID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.handlers, pid)
}

// Send delivers a signal to a process.
// For SIGTERM: sets state to blocked, invokes handlers (agent gets grace period).
// For SIGKILL: sets state to dead immediately.
// For SIGCHLD: delivered to parent with exit info.
func (r *SignalRouter) Send(pid PID, sig Signal, info *ExitInfo) error {
	proc, err := r.registry.Get(pid)
	if err != nil {
		return fmt.Errorf("signal %s to PID %d: %w", sig, pid, err)
	}

	log.Printf("[signal] %s -> PID %d (%s)", sig, pid, proc.Name)

	switch sig {
	case SIGKILL:
		// Immediate death, no handler invocation.
		_ = r.registry.SetState(pid, StateDead)
		r.invokeHandlers(pid, sig, info)
		return nil

	case SIGTERM:
		// Mark as blocked (shutting down), then invoke handlers.
		_ = r.registry.SetState(pid, StateBlocked)
		r.invokeHandlers(pid, sig, info)
		return nil

	case SIGCHLD:
		// Delivered to a parent when a child exits.
		r.invokeHandlers(pid, sig, info)
		return nil

	case SIGSTOP:
		_ = r.registry.SetState(pid, StateSleeping)
		r.invokeHandlers(pid, sig, info)
		return nil

	case SIGCONT:
		_ = r.registry.SetState(pid, StateRunning)
		r.invokeHandlers(pid, sig, info)
		return nil

	case SIGHUP:
		r.invokeHandlers(pid, sig, info)
		return nil

	default:
		r.invokeHandlers(pid, sig, info)
		return nil
	}
}

// SendWithGrace sends SIGTERM, waits for grace period, then sends SIGKILL
// if the process is still alive.
func (r *SignalRouter) SendWithGrace(pid PID, grace time.Duration) {
	if err := r.Send(pid, SIGTERM, nil); err != nil {
		log.Printf("[signal] SIGTERM failed for PID %d: %v", pid, err)
		return
	}

	go func() {
		time.Sleep(grace)
		proc, err := r.registry.Get(pid)
		if err != nil {
			return // already removed
		}
		if proc.State != StateDead {
			log.Printf("[signal] PID %d did not exit after %s, sending SIGKILL", pid, grace)
			_ = r.Send(pid, SIGKILL, nil)
		}
	}()
}

// NotifyParent sends SIGCHLD to the parent of the exited process.
func (r *SignalRouter) NotifyParent(exitedPID PID, exitCode int, output string) {
	proc, err := r.registry.Get(exitedPID)
	if err != nil {
		return
	}
	if proc.PPID == 0 {
		return // kernel has no parent
	}

	info := &ExitInfo{
		PID:      exitedPID,
		ExitCode: exitCode,
		Output:   output,
		ExitedAt: time.Now(),
	}
	_ = r.Send(proc.PPID, SIGCHLD, info)
}

func (r *SignalRouter) invokeHandlers(pid PID, sig Signal, info *ExitInfo) {
	r.mu.RLock()
	handlers := r.handlers[pid]
	r.mu.RUnlock()

	for _, h := range handlers {
		h(pid, sig, info)
	}
}
