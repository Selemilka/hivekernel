package runtime

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/selemilka/hivekernel/internal/process"
)

// HealthMonitor periodically checks agent heartbeats and marks
// unresponsive agents as zombies.
type HealthMonitor struct {
	registry    *process.Registry
	manager     *Manager
	interval    time.Duration
	timeout     time.Duration
	lastSeen    map[process.PID]time.Time
	mu          sync.Mutex
	onUnhealthy func(pid process.PID) // callback when agent is unresponsive
}

// NewHealthMonitor creates a new health monitor.
func NewHealthMonitor(
	registry *process.Registry,
	manager *Manager,
	interval time.Duration,
	timeout time.Duration,
) *HealthMonitor {
	return &HealthMonitor{
		registry: registry,
		manager:  manager,
		interval: interval,
		timeout:  timeout,
		lastSeen: make(map[process.PID]time.Time),
	}
}

// OnUnhealthy sets a callback invoked when an agent becomes unresponsive.
func (h *HealthMonitor) OnUnhealthy(fn func(pid process.PID)) {
	h.onUnhealthy = fn
}

// RecordHeartbeat records that an agent is alive.
func (h *HealthMonitor) RecordHeartbeat(pid process.PID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastSeen[pid] = time.Now()
}

// Run starts the health check loop. Blocks until ctx is cancelled.
func (h *HealthMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.check()
		}
	}
}

func (h *HealthMonitor) check() {
	h.mu.Lock()
	defer h.mu.Unlock()

	runtimes := h.manager.ListRuntimes()
	now := time.Now()

	for _, rt := range runtimes {
		pid := rt.PID
		lastSeen, ok := h.lastSeen[pid]
		if !ok {
			// First check — give it a grace period.
			h.lastSeen[pid] = rt.ProcessInfo.StartedAt
			continue
		}

		if now.Sub(lastSeen) > h.timeout {
			log.Printf("[health] PID %d (%s) unresponsive for %s, marking zombie",
				pid, rt.ProcessInfo.Name, now.Sub(lastSeen).Round(time.Second))

			_ = h.registry.SetState(pid, process.StateZombie)

			if h.onUnhealthy != nil {
				h.onUnhealthy(pid)
			}
		}
	}
}

// Remove cleans up tracking for a stopped process.
func (h *HealthMonitor) Remove(pid process.PID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.lastSeen, pid)
}
