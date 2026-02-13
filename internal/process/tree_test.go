package process

import (
	"testing"
	"time"
)

func setupTree(t *testing.T) (*Registry, *SignalRouter, *TreeOps) {
	t.Helper()
	r := NewRegistry()
	sr := NewSignalRouter(r)
	tree := NewTreeOps(r, sr)

	// Build: king(1) -> queen(2) -> [worker-a(3), worker-b(4)]
	//                                worker-a(3) -> task(5)
	king := &Process{Name: "king", User: "root", Role: RoleKernel, State: StateRunning}
	r.Register(king) // PID 1

	queen := &Process{PPID: 1, Name: "queen", User: "root", Role: RoleDaemon, State: StateRunning}
	r.Register(queen) // PID 2

	wa := &Process{PPID: 2, Name: "worker-a", User: "root", Role: RoleWorker, State: StateRunning}
	r.Register(wa) // PID 3

	wb := &Process{PPID: 2, Name: "worker-b", User: "root", Role: RoleWorker, State: StateRunning}
	r.Register(wb) // PID 4

	task := &Process{PPID: 3, Name: "task", User: "root", Role: RoleTask, State: StateRunning}
	r.Register(task) // PID 5

	return r, sr, tree
}

func TestKillBranch(t *testing.T) {
	r, _, tree := setupTree(t)

	// Kill queen's branch (PID 2). Should kill task(5), worker-a(3), worker-b(4), queen(2).
	killed, err := tree.KillBranch(2, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("KillBranch: %v", err)
	}

	// Should have killed at least task + 2 workers + queen = 4.
	if len(killed) < 4 {
		t.Fatalf("expected at least 4 killed, got %d: %v", len(killed), killed)
	}

	// After grace period, all should be dead.
	time.Sleep(100 * time.Millisecond)
	for _, pid := range killed {
		p, err := r.Get(pid)
		if err != nil {
			continue
		}
		if p.State != StateDead {
			t.Errorf("PID %d (%s) should be dead, got %s", pid, p.Name, p.State)
		}
	}

	// King should still be alive.
	king, _ := r.Get(1)
	if king.State != StateRunning {
		t.Fatalf("king should still be running, got %s", king.State)
	}
}

func TestReparent(t *testing.T) {
	r, _, tree := setupTree(t)

	// Reparent worker-a (PID 3) from queen (PID 2) to king (PID 1).
	err := tree.Reparent(3, 1)
	if err != nil {
		t.Fatalf("Reparent: %v", err)
	}

	wa, _ := r.Get(3)
	if wa.PPID != 1 {
		t.Fatalf("expected PPID 1, got %d", wa.PPID)
	}

	// King should now have worker-a as child.
	children := r.GetChildren(1)
	found := false
	for _, c := range children {
		if c.PID == 3 {
			found = true
		}
	}
	if !found {
		t.Fatal("worker-a not found in king's children after reparent")
	}
}

func TestOrphanAdoption(t *testing.T) {
	r, _, tree := setupTree(t)

	// Kill queen, making workers orphans.
	_ = r.SetState(2, StateDead)

	// King adopts orphans.
	adopted := tree.OrphanAdoption(1)

	if len(adopted) < 2 {
		t.Fatalf("expected at least 2 adopted, got %d", len(adopted))
	}

	// worker-a and worker-b should now be under king.
	for _, pid := range adopted {
		p, _ := r.Get(pid)
		if p.PPID != 1 {
			t.Errorf("PID %d should have PPID 1, got %d", pid, p.PPID)
		}
	}
}
