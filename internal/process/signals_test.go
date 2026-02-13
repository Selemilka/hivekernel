package process

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestSignalSIGTERM(t *testing.T) {
	r := NewRegistry()
	sr := NewSignalRouter(r)

	p := &Process{Name: "test", State: StateRunning}
	r.Register(p)

	var called atomic.Bool
	sr.Register(p.PID, func(pid PID, sig Signal, info *ExitInfo) {
		called.Store(true)
	})

	err := sr.Send(p.PID, SIGTERM, nil)
	if err != nil {
		t.Fatalf("SIGTERM failed: %v", err)
	}

	// Process should be in Blocked state (shutting down).
	got, _ := r.Get(p.PID)
	if got.State != StateBlocked {
		t.Fatalf("expected StateBlocked after SIGTERM, got %s", got.State)
	}
	if !called.Load() {
		t.Fatal("handler was not called")
	}
}

func TestSignalSIGKILL(t *testing.T) {
	r := NewRegistry()
	sr := NewSignalRouter(r)

	p := &Process{Name: "test", State: StateRunning}
	r.Register(p)

	err := sr.Send(p.PID, SIGKILL, nil)
	if err != nil {
		t.Fatalf("SIGKILL failed: %v", err)
	}

	got, _ := r.Get(p.PID)
	if got.State != StateDead {
		t.Fatalf("expected StateDead after SIGKILL, got %s", got.State)
	}
}

func TestSignalSIGCHLD(t *testing.T) {
	r := NewRegistry()
	sr := NewSignalRouter(r)

	parent := &Process{Name: "parent", State: StateRunning}
	r.Register(parent)

	child := &Process{PPID: parent.PID, Name: "child", State: StateRunning}
	r.Register(child)

	var receivedInfo *ExitInfo
	sr.Register(parent.PID, func(pid PID, sig Signal, info *ExitInfo) {
		if sig == SIGCHLD {
			receivedInfo = info
		}
	})

	sr.NotifyParent(child.PID, 0, "task done")

	if receivedInfo == nil {
		t.Fatal("parent did not receive SIGCHLD")
	}
	if receivedInfo.PID != child.PID {
		t.Fatalf("expected child PID %d, got %d", child.PID, receivedInfo.PID)
	}
	if receivedInfo.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", receivedInfo.ExitCode)
	}
}

func TestSendWithGrace(t *testing.T) {
	r := NewRegistry()
	sr := NewSignalRouter(r)

	p := &Process{Name: "slow", State: StateRunning}
	r.Register(p)

	sr.SendWithGrace(p.PID, 100*time.Millisecond)

	// Immediately: should be Blocked (SIGTERM sent).
	got, _ := r.Get(p.PID)
	if got.State != StateBlocked {
		t.Fatalf("expected StateBlocked immediately, got %s", got.State)
	}

	// After grace period: should be Dead (SIGKILL sent).
	time.Sleep(200 * time.Millisecond)
	got, _ = r.Get(p.PID)
	if got.State != StateDead {
		t.Fatalf("expected StateDead after grace, got %s", got.State)
	}
}

func TestSignalSTOP_CONT(t *testing.T) {
	r := NewRegistry()
	sr := NewSignalRouter(r)

	p := &Process{Name: "test", State: StateRunning}
	r.Register(p)

	_ = sr.Send(p.PID, SIGSTOP, nil)
	got, _ := r.Get(p.PID)
	if got.State != StateSleeping {
		t.Fatalf("expected StateSleeping after SIGSTOP, got %s", got.State)
	}

	_ = sr.Send(p.PID, SIGCONT, nil)
	got, _ = r.Get(p.PID)
	if got.State != StateRunning {
		t.Fatalf("expected StateRunning after SIGCONT, got %s", got.State)
	}
}
