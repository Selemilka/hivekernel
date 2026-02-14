package runtime

import (
	"context"
	"log"
	"sync"
	"time"

	pb "github.com/selemilka/hivekernel/api/proto/hivepb"
	"github.com/selemilka/hivekernel/internal/process"
)

// HealthMonitor periodically pings agent runtimes via gRPC Heartbeat RPC
// and reports unreachable agents through the onUnhealthy callback.
// It never changes process state directly — the callback decides what to do.
type HealthMonitor struct {
	manager     *Manager
	interval    time.Duration
	maxFailures int
	pingTimeout time.Duration

	mu          sync.Mutex
	failures    map[process.PID]int  // consecutive ping failures
	onUnhealthy func(pid process.PID) // callback when agent is unreachable
}

// NewHealthMonitor creates a health monitor that pings agents every interval.
// An agent is considered unreachable after maxFailures consecutive failed pings.
func NewHealthMonitor(
	manager *Manager,
	interval time.Duration,
	maxFailures int,
	pingTimeout time.Duration,
) *HealthMonitor {
	return &HealthMonitor{
		manager:     manager,
		interval:    interval,
		maxFailures: maxFailures,
		pingTimeout: pingTimeout,
		failures:    make(map[process.PID]int),
	}
}

// OnUnhealthy sets a callback invoked when an agent becomes unreachable
// (after maxFailures consecutive failed pings).
func (h *HealthMonitor) OnUnhealthy(fn func(pid process.PID)) {
	h.onUnhealthy = fn
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
			h.check(ctx)
		}
	}
}

func (h *HealthMonitor) check(ctx context.Context) {
	runtimes := h.manager.ListRuntimes()

	// Ping all agents concurrently.
	type pingResult struct {
		pid process.PID
		ok  bool
	}

	results := make(chan pingResult, len(runtimes))
	for _, rt := range runtimes {
		go func(rt *AgentRuntime) {
			ok := h.ping(ctx, rt)
			results <- pingResult{pid: rt.PID, ok: ok}
		}(rt)
	}

	// Collect results.
	for range runtimes {
		r := <-results
		h.mu.Lock()
		if r.ok {
			delete(h.failures, r.pid)
		} else {
			h.failures[r.pid]++
			count := h.failures[r.pid]
			if count >= h.maxFailures {
				log.Printf("[health] PID %d unreachable (%d consecutive failures)", r.pid, count)
				if h.onUnhealthy != nil {
					h.onUnhealthy(r.pid)
				}
				// Reset so we don't spam the callback every tick.
				delete(h.failures, r.pid)
			}
		}
		h.mu.Unlock()
	}
}

// ping calls the Heartbeat RPC on an agent runtime.
func (h *HealthMonitor) ping(ctx context.Context, rt *AgentRuntime) bool {
	if rt.Client == nil {
		return false
	}

	pingCtx, cancel := context.WithTimeout(ctx, h.pingTimeout)
	defer cancel()

	_, err := rt.Client.Heartbeat(pingCtx, &pb.HeartbeatRequest{
		Timestamp: uint64(time.Now().UnixMilli()),
	})
	return err == nil
}

// Remove cleans up tracking for a stopped process.
func (h *HealthMonitor) Remove(pid process.PID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.failures, pid)
}
