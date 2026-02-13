package process

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// TaskResult captures the output of a completed task.
type TaskResult struct {
	PID       PID
	ExitCode  int
	Output    []byte
	Error     string
	Duration  time.Duration
	Timestamp time.Time
}

// LifecycleManager orchestrates dynamic process lifecycle:
// sleep/wake, branch collapse on task completion, and result collection.
type LifecycleManager struct {
	mu       sync.RWMutex
	registry *Registry
	signals  *SignalRouter
	results  map[PID]*TaskResult // collected results (like waitpid)
}

// NewLifecycleManager creates a lifecycle manager.
func NewLifecycleManager(registry *Registry, signals *SignalRouter) *LifecycleManager {
	return &LifecycleManager{
		registry: registry,
		signals:  signals,
		results:  make(map[PID]*TaskResult),
	}
}

// Sleep puts an agent into sleeping state.
// Used for architect-type roles that produce artifacts then sleep.
func (lm *LifecycleManager) Sleep(pid PID) error {
	proc, err := lm.registry.Get(pid)
	if err != nil {
		return fmt.Errorf("sleep: %w", err)
	}

	if proc.State == StateDead {
		return fmt.Errorf("sleep: PID %d is dead", pid)
	}
	if proc.State == StateSleeping {
		return nil // already sleeping, idempotent
	}

	err = lm.registry.SetState(pid, StateSleeping)
	if err != nil {
		return fmt.Errorf("sleep: %w", err)
	}

	log.Printf("[lifecycle] PID %d (%s) → sleeping", pid, proc.Name)
	return nil
}

// Wake wakes a sleeping agent, setting it to idle (ready for tasks).
func (lm *LifecycleManager) Wake(pid PID) error {
	proc, err := lm.registry.Get(pid)
	if err != nil {
		return fmt.Errorf("wake: %w", err)
	}

	if proc.State != StateSleeping {
		return fmt.Errorf("wake: PID %d is %s, not sleeping", pid, proc.State)
	}

	// Send SIGCONT (sets state to Running via signal handler).
	_ = lm.signals.Send(pid, SIGCONT, nil)

	// Override to Idle — process is ready but not yet executing a task.
	err = lm.registry.SetState(pid, StateIdle)
	if err != nil {
		return fmt.Errorf("wake: %w", err)
	}

	log.Printf("[lifecycle] PID %d (%s) → woken", pid, proc.Name)
	return nil
}

// CompleteTask records a task completion and marks the process as dead.
// Returns the result for the parent to collect.
func (lm *LifecycleManager) CompleteTask(pid PID, exitCode int, output []byte, errMsg string) (*TaskResult, error) {
	proc, err := lm.registry.Get(pid)
	if err != nil {
		return nil, fmt.Errorf("complete: %w", err)
	}

	result := &TaskResult{
		PID:       pid,
		ExitCode:  exitCode,
		Output:    output,
		Error:     errMsg,
		Duration:  time.Since(proc.StartedAt),
		Timestamp: time.Now(),
	}

	lm.mu.Lock()
	lm.results[pid] = result
	lm.mu.Unlock()

	// Mark as dead.
	_ = lm.registry.SetState(pid, StateDead)

	// Notify parent via SIGCHLD.
	_ = lm.signals.Send(proc.PPID, SIGCHLD, nil)

	log.Printf("[lifecycle] PID %d (%s) completed: exit=%d", pid, proc.Name, exitCode)
	return result, nil
}

// WaitResult retrieves the result from a completed child (like waitpid).
// Removes the result after retrieval.
func (lm *LifecycleManager) WaitResult(pid PID) (*TaskResult, bool) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	result, ok := lm.results[pid]
	if ok {
		delete(lm.results, pid)
	}
	return result, ok
}

// PeekResult retrieves the result without removing it.
func (lm *LifecycleManager) PeekResult(pid PID) (*TaskResult, bool) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	result, ok := lm.results[pid]
	return result, ok
}

// PendingResults returns the number of uncollected results.
func (lm *LifecycleManager) PendingResults() int {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	return len(lm.results)
}

// CollapseBranch kills all children of a process and collects their results.
// Used when a lead's task completes: kill all workers, collect outputs.
func (lm *LifecycleManager) CollapseBranch(parentPID PID) ([]TaskResult, error) {
	children := lm.registry.GetChildren(parentPID)
	if len(children) == 0 {
		return nil, nil
	}

	var results []TaskResult

	// Use TreeOps for proper bottom-up kill ordering.
	treeOps := NewTreeOps(lm.registry, lm.signals)

	for _, child := range children {
		if child.State == StateDead {
			// Already dead, just collect result.
			if r, ok := lm.WaitResult(child.PID); ok {
				results = append(results, *r)
			}
			continue
		}

		// Kill the child's entire subtree first.
		descendants := lm.registry.GetDescendants(child.PID)
		for _, d := range treeOps.bottomUpOrder(child.PID, descendants) {
			if d.State != StateDead {
				_ = lm.signals.Send(d.PID, SIGTERM, nil)
				_ = lm.registry.SetState(d.PID, StateDead)
			}
		}

		// Kill the child itself.
		_ = lm.signals.Send(child.PID, SIGTERM, nil)
		_ = lm.registry.SetState(child.PID, StateDead)

		// Record a synthetic result if none exists.
		if _, exists := lm.PeekResult(child.PID); !exists {
			lm.mu.Lock()
			lm.results[child.PID] = &TaskResult{
				PID:       child.PID,
				ExitCode:  -1, // killed, no clean exit
				Error:     "branch collapsed",
				Timestamp: time.Now(),
			}
			lm.mu.Unlock()
		}

		if r, ok := lm.WaitResult(child.PID); ok {
			results = append(results, *r)
		}
	}

	log.Printf("[lifecycle] collapsed branch under PID %d: %d children", parentPID, len(results))
	return results, nil
}

// IsAlive checks if a process is in a running/idle/blocked state (not dead/sleeping).
func (lm *LifecycleManager) IsAlive(pid PID) bool {
	proc, err := lm.registry.Get(pid)
	if err != nil {
		return false
	}
	return proc.State != StateDead && proc.State != StateZombie
}

// ActiveChildren returns the number of alive (non-dead, non-zombie) children.
func (lm *LifecycleManager) ActiveChildren(parentPID PID) int {
	children := lm.registry.GetChildren(parentPID)
	count := 0
	for _, c := range children {
		if c.State != StateDead && c.State != StateZombie {
			count++
		}
	}
	return count
}
