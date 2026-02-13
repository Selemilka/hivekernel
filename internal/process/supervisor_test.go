package process

import (
	"context"
	"testing"
	"time"
)

func TestSupervisorHandleChildExit_Daemon(t *testing.T) {
	r := NewRegistry()
	sr := NewSignalRouter(r)
	tree := NewTreeOps(r, sr)
	cfg := DefaultSupervisorConfig()
	cfg.RestartBackoff = 10 * time.Millisecond
	sup := NewSupervisor(r, sr, tree, cfg)

	restarted := make(chan PID, 1)
	sup.OnRestart(func(proc *Process) error {
		restarted <- proc.PID
		return nil
	})

	// Create a daemon.
	daemon := &Process{Name: "queen", Role: RoleDaemon, State: StateRunning}
	r.Register(daemon)

	// Simulate crash.
	sup.HandleChildExit(daemon.PID, 1)

	// Should be restarted (RestartAlways for daemons).
	select {
	case pid := <-restarted:
		if pid != daemon.PID {
			t.Fatalf("expected restart of PID %d, got %d", daemon.PID, pid)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("daemon was not restarted")
	}
}

func TestSupervisorHandleChildExit_Task(t *testing.T) {
	r := NewRegistry()
	sr := NewSignalRouter(r)
	tree := NewTreeOps(r, sr)
	sup := NewSupervisor(r, sr, tree, DefaultSupervisorConfig())

	parent := &Process{Name: "parent", Role: RoleWorker, State: StateRunning}
	r.Register(parent)

	task := &Process{PPID: parent.PID, Name: "task", Role: RoleTask, State: StateRunning}
	r.Register(task)

	// Track SIGCHLD to parent.
	var childExitPID PID
	sr.Register(parent.PID, func(pid PID, sig Signal, info *ExitInfo) {
		if sig == SIGCHLD && info != nil {
			childExitPID = info.PID
		}
	})

	// Task exits.
	sup.HandleChildExit(task.PID, 0)

	// Task should be dead (RestartNever).
	got, _ := r.Get(task.PID)
	if got.State != StateDead {
		t.Fatalf("expected task to be dead, got %s", got.State)
	}

	// Parent should have been notified.
	if childExitPID != task.PID {
		t.Fatalf("parent should have received SIGCHLD for PID %d, got %d", task.PID, childExitPID)
	}
}

func TestSupervisorZombieReaping(t *testing.T) {
	r := NewRegistry()
	sr := NewSignalRouter(r)
	tree := NewTreeOps(r, sr)
	cfg := DefaultSupervisorConfig()
	cfg.ZombieScanInterval = 10 * time.Millisecond
	cfg.ZombieTimeout = 20 * time.Millisecond
	sup := NewSupervisor(r, sr, tree, cfg)

	parent := &Process{Name: "parent", State: StateRunning}
	r.Register(parent)

	zombie := &Process{PPID: parent.PID, Name: "zombie", State: StateZombie}
	r.Register(zombie)
	// Backdate UpdatedAt so it looks old enough to reap.
	_ = r.Update(zombie.PID, func(p *Process) {
		p.UpdatedAt = time.Now().Add(-1 * time.Minute)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	go sup.Run(ctx)
	time.Sleep(100 * time.Millisecond)

	got, _ := r.Get(zombie.PID)
	if got.State != StateDead {
		t.Fatalf("zombie should have been reaped, state=%s", got.State)
	}
}

func TestSupervisorMaxRestarts(t *testing.T) {
	r := NewRegistry()
	sr := NewSignalRouter(r)
	tree := NewTreeOps(r, sr)
	cfg := DefaultSupervisorConfig()
	cfg.MaxRestartAttempts = 2
	cfg.RestartBackoff = 5 * time.Millisecond
	sup := NewSupervisor(r, sr, tree, cfg)

	restartCount := 0
	sup.OnRestart(func(proc *Process) error {
		restartCount++
		return nil
	})

	daemon := &Process{Name: "flaky", Role: RoleDaemon, State: StateRunning}
	r.Register(daemon)

	// Crash 3 times. Max is 2, so third should not restart.
	sup.HandleChildExit(daemon.PID, 1) // attempt 1
	time.Sleep(50 * time.Millisecond)
	sup.HandleChildExit(daemon.PID, 1) // attempt 2
	time.Sleep(50 * time.Millisecond)
	sup.HandleChildExit(daemon.PID, 1) // should give up
	time.Sleep(50 * time.Millisecond)

	if restartCount != 2 {
		t.Fatalf("expected 2 restarts, got %d", restartCount)
	}

	// Should be dead now.
	got, _ := r.Get(daemon.PID)
	if got.State != StateDead {
		t.Fatalf("expected dead after max restarts, got %s", got.State)
	}
}
