package resources

import (
	"testing"
	"time"

	"github.com/selemilka/hivekernel/internal/process"
)

func TestRateLimiterAllow(t *testing.T) {
	rl := NewRateLimiter()
	rl.SetLimit(1, 3) // 3 calls per minute

	now := time.Now()

	if !rl.AllowAt(1, now) {
		t.Fatal("first call should be allowed")
	}
	if !rl.AllowAt(1, now.Add(time.Second)) {
		t.Fatal("second call should be allowed")
	}
	if !rl.AllowAt(1, now.Add(2*time.Second)) {
		t.Fatal("third call should be allowed")
	}
	if rl.AllowAt(1, now.Add(3*time.Second)) {
		t.Fatal("fourth call should be rejected (rate limit)")
	}
}

func TestRateLimiterWindowExpiry(t *testing.T) {
	rl := NewRateLimiter()
	rl.SetLimit(1, 2)

	now := time.Now()
	rl.AllowAt(1, now)
	rl.AllowAt(1, now.Add(time.Second))

	// 4th call at +3s should be denied.
	if rl.AllowAt(1, now.Add(3*time.Second)) {
		t.Fatal("should be rate limited")
	}

	// After 1 minute, window expires.
	if !rl.AllowAt(1, now.Add(61*time.Second)) {
		t.Fatal("should be allowed after window expires")
	}
}

func TestRateLimiterNoLimit(t *testing.T) {
	rl := NewRateLimiter()

	// PID 99 has no limit configured — should always allow.
	for i := 0; i < 100; i++ {
		if !rl.Allow(99) {
			t.Fatal("should always allow when no limit configured")
		}
	}
}

func TestRateLimiterRemove(t *testing.T) {
	rl := NewRateLimiter()
	rl.SetLimit(1, 1)
	rl.Allow(1)

	rl.Remove(1)

	// After remove, no limit — should allow.
	if !rl.Allow(1) {
		t.Fatal("should allow after remove")
	}
}

func TestLimitCheckerSpawn(t *testing.T) {
	r := process.NewRegistry()
	r.Register(&process.Process{
		Name: "parent", User: "root", Role: process.RoleDaemon,
		State: process.StateRunning,
		Limits: process.ResourceLimits{MaxChildren: 2},
	})
	r.Register(&process.Process{PPID: 1, Name: "child1", User: "root", Role: process.RoleWorker})
	r.Register(&process.Process{PPID: 1, Name: "child2", User: "root", Role: process.RoleWorker})

	lc := NewLimitChecker(r)

	err := lc.CheckSpawnLimits(1)
	if err == nil {
		t.Fatal("expected error: max children exceeded")
	}
}

func TestLimitCheckerContextWindow(t *testing.T) {
	r := process.NewRegistry()
	r.Register(&process.Process{
		Name: "worker", User: "root", Role: process.RoleWorker,
		Limits: process.ResourceLimits{MaxContextTokens: 128000},
	})

	lc := NewLimitChecker(r)

	// Under limit.
	err := lc.CheckContextWindow(1, 50000)
	if err != nil {
		t.Fatalf("should be within limit: %v", err)
	}

	// Over limit.
	err = lc.CheckContextWindow(1, 200000)
	if err == nil {
		t.Fatal("expected error for context window exceeded")
	}
}

func TestLimitCheckerTimeout(t *testing.T) {
	r := process.NewRegistry()
	r.Register(&process.Process{
		Name: "task", User: "root", Role: process.RoleTask,
		Limits: process.ResourceLimits{TimeoutSeconds: 1},
	})

	lc := NewLimitChecker(r)

	// Just created — should not be exceeded.
	exceeded, err := lc.CheckTimeout(1)
	if err != nil {
		t.Fatalf("CheckTimeout: %v", err)
	}
	if exceeded {
		t.Fatal("should not be exceeded immediately after creation")
	}
}
