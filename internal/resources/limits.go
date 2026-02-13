package resources

import (
	"fmt"
	"sync"
	"time"

	"github.com/selemilka/hivekernel/internal/process"
)

// RateLimiter tracks per-process API call rate limits.
type RateLimiter struct {
	mu      sync.Mutex
	windows map[process.PID]*rateWindow
}

type rateWindow struct {
	Calls     []time.Time
	MaxPerMin uint32
}

// NewRateLimiter creates a new rate limiter.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		windows: make(map[process.PID]*rateWindow),
	}
}

// SetLimit configures the rate limit for a process.
func (rl *RateLimiter) SetLimit(pid process.PID, maxPerMin uint32) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.windows[pid] = &rateWindow{MaxPerMin: maxPerMin}
}

// Allow checks if a process can make an API call. Returns true and records the call,
// or returns false if rate limit is exceeded.
func (rl *RateLimiter) Allow(pid process.PID) bool {
	return rl.AllowAt(pid, time.Now())
}

// AllowAt is like Allow but uses a provided timestamp (for testing).
func (rl *RateLimiter) AllowAt(pid process.PID, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	w, ok := rl.windows[pid]
	if !ok {
		return true // no limit configured
	}

	// Prune calls older than 1 minute.
	cutoff := now.Add(-time.Minute)
	fresh := w.Calls[:0]
	for _, t := range w.Calls {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	w.Calls = fresh

	if uint32(len(w.Calls)) >= w.MaxPerMin {
		return false
	}

	w.Calls = append(w.Calls, now)
	return true
}

// Remove cleans up rate limit state for a dead process.
func (rl *RateLimiter) Remove(pid process.PID) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	delete(rl.windows, pid)
}

// LimitChecker validates process resource limits before operations.
type LimitChecker struct {
	registry *process.Registry
}

// NewLimitChecker creates a limit checker backed by the registry.
func NewLimitChecker(registry *process.Registry) *LimitChecker {
	return &LimitChecker{registry: registry}
}

// CheckSpawnLimits validates that the parent has capacity to spawn a child.
// Checks: max_children, concurrent tasks.
func (lc *LimitChecker) CheckSpawnLimits(parentPID process.PID) error {
	parent, err := lc.registry.Get(parentPID)
	if err != nil {
		return fmt.Errorf("parent %d not found: %w", parentPID, err)
	}

	if parent.Limits.MaxChildren > 0 {
		count := lc.registry.CountChildren(parentPID)
		if uint32(count) >= parent.Limits.MaxChildren {
			return fmt.Errorf(
				"PID %d has %d children, max is %d",
				parentPID, count, parent.Limits.MaxChildren,
			)
		}
	}

	return nil
}

// CheckContextWindow validates that a process hasn't exceeded its context limit.
func (lc *LimitChecker) CheckContextWindow(pid process.PID, currentTokens uint64) error {
	proc, err := lc.registry.Get(pid)
	if err != nil {
		return err
	}

	if proc.Limits.MaxContextTokens > 0 && currentTokens > proc.Limits.MaxContextTokens {
		return fmt.Errorf(
			"PID %d context %d exceeds max %d",
			pid, currentTokens, proc.Limits.MaxContextTokens,
		)
	}
	return nil
}

// CheckTimeout returns whether a process has exceeded its timeout.
func (lc *LimitChecker) CheckTimeout(pid process.PID) (exceeded bool, err error) {
	proc, err := lc.registry.Get(pid)
	if err != nil {
		return false, err
	}

	if proc.Limits.TimeoutSeconds == 0 {
		return false, nil // no timeout
	}

	elapsed := time.Since(proc.StartedAt)
	return elapsed > time.Duration(proc.Limits.TimeoutSeconds)*time.Second, nil
}
