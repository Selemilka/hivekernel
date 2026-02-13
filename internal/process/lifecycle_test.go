package process

import (
	"testing"
)

func setupLifecycleTest() (*LifecycleManager, *Registry, *Spawner) {
	reg := NewRegistry()
	signals := NewSignalRouter(reg)
	spawner := NewSpawner(reg)
	lm := NewLifecycleManager(reg, signals)

	// king -> queen -> [worker1, worker2, architect(sleeping)]
	spawner.SpawnKernel("king", "root", "vps1")
	spawner.Spawn(SpawnRequest{
		ParentPID:     1,
		Name:          "queen",
		Role:          RoleDaemon,
		CognitiveTier: CogTactical,
		Limits:        ResourceLimits{MaxChildren: 10},
	})
	spawner.Spawn(SpawnRequest{
		ParentPID:     2,
		Name:          "worker1",
		Role:          RoleWorker,
		CognitiveTier: CogTactical,
		Limits:        ResourceLimits{MaxChildren: 5},
	})
	spawner.Spawn(SpawnRequest{
		ParentPID:     2,
		Name:          "worker2",
		Role:          RoleWorker,
		CognitiveTier: CogTactical,
		Limits:        ResourceLimits{MaxChildren: 5},
	})
	spawner.Spawn(SpawnRequest{
		ParentPID:     2,
		Name:          "architect",
		Role:          RoleArchitect,
		CognitiveTier: CogTactical,
		Limits:        ResourceLimits{MaxChildren: 5},
	})

	return lm, reg, spawner
}

func TestLifecycleManager_Sleep(t *testing.T) {
	lm, reg, _ := setupLifecycleTest()

	arch, _ := reg.GetByName("architect")
	_ = reg.SetState(arch.PID, StateRunning)

	err := lm.Sleep(arch.PID)
	if err != nil {
		t.Fatalf("Sleep: %v", err)
	}

	p, _ := reg.Get(arch.PID)
	if p.State != StateSleeping {
		t.Errorf("State=%s, want sleeping", p.State)
	}
}

func TestLifecycleManager_Sleep_Idempotent(t *testing.T) {
	lm, reg, _ := setupLifecycleTest()

	arch, _ := reg.GetByName("architect")
	_ = reg.SetState(arch.PID, StateSleeping)

	err := lm.Sleep(arch.PID)
	if err != nil {
		t.Fatalf("idempotent Sleep should not fail: %v", err)
	}
}

func TestLifecycleManager_Sleep_Dead(t *testing.T) {
	lm, reg, _ := setupLifecycleTest()

	arch, _ := reg.GetByName("architect")
	_ = reg.SetState(arch.PID, StateDead)

	err := lm.Sleep(arch.PID)
	if err == nil {
		t.Error("sleeping a dead process should fail")
	}
}

func TestLifecycleManager_Wake(t *testing.T) {
	lm, reg, _ := setupLifecycleTest()

	arch, _ := reg.GetByName("architect")
	_ = reg.SetState(arch.PID, StateSleeping)

	err := lm.Wake(arch.PID)
	if err != nil {
		t.Fatalf("Wake: %v", err)
	}

	p, _ := reg.Get(arch.PID)
	if p.State != StateIdle {
		t.Errorf("State=%s, want idle after wake", p.State)
	}
}

func TestLifecycleManager_Wake_NotSleeping(t *testing.T) {
	lm, reg, _ := setupLifecycleTest()

	w, _ := reg.GetByName("worker1")
	_ = reg.SetState(w.PID, StateRunning)

	err := lm.Wake(w.PID)
	if err == nil {
		t.Error("waking a running process should fail")
	}
}

func TestLifecycleManager_CompleteTask(t *testing.T) {
	lm, reg, _ := setupLifecycleTest()

	w, _ := reg.GetByName("worker1")
	_ = reg.SetState(w.PID, StateRunning)

	result, err := lm.CompleteTask(w.PID, 0, []byte("result data"), "")
	if err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}

	if result.ExitCode != 0 {
		t.Errorf("ExitCode=%d, want 0", result.ExitCode)
	}
	if string(result.Output) != "result data" {
		t.Errorf("Output=%q", result.Output)
	}

	p, _ := reg.Get(w.PID)
	if p.State != StateDead {
		t.Errorf("State=%s, want dead after completion", p.State)
	}
}

func TestLifecycleManager_CompleteTask_Failure(t *testing.T) {
	lm, reg, _ := setupLifecycleTest()

	w, _ := reg.GetByName("worker1")
	_ = reg.SetState(w.PID, StateRunning)

	result, _ := lm.CompleteTask(w.PID, 1, nil, "compilation failed")
	if result.ExitCode != 1 {
		t.Errorf("ExitCode=%d, want 1", result.ExitCode)
	}
	if result.Error != "compilation failed" {
		t.Errorf("Error=%q", result.Error)
	}
}

func TestLifecycleManager_WaitResult(t *testing.T) {
	lm, reg, _ := setupLifecycleTest()

	w, _ := reg.GetByName("worker1")
	_ = reg.SetState(w.PID, StateRunning)
	lm.CompleteTask(w.PID, 0, []byte("output"), "")

	// First wait should succeed.
	result, ok := lm.WaitResult(w.PID)
	if !ok || result == nil {
		t.Fatal("WaitResult should find the result")
	}
	if string(result.Output) != "output" {
		t.Errorf("Output=%q", result.Output)
	}

	// Second wait should find nothing (consumed).
	_, ok = lm.WaitResult(w.PID)
	if ok {
		t.Error("WaitResult should not find result again (consumed)")
	}
}

func TestLifecycleManager_PeekResult(t *testing.T) {
	lm, reg, _ := setupLifecycleTest()

	w, _ := reg.GetByName("worker1")
	_ = reg.SetState(w.PID, StateRunning)
	lm.CompleteTask(w.PID, 0, nil, "")

	// Peek does not consume.
	r1, ok := lm.PeekResult(w.PID)
	if !ok {
		t.Fatal("PeekResult should find result")
	}
	r2, ok := lm.PeekResult(w.PID)
	if !ok {
		t.Fatal("PeekResult should still find result (not consumed)")
	}
	if r1.PID != r2.PID {
		t.Error("same result should be returned")
	}
}

func TestLifecycleManager_PendingResults(t *testing.T) {
	lm, reg, _ := setupLifecycleTest()

	w1, _ := reg.GetByName("worker1")
	w2, _ := reg.GetByName("worker2")
	_ = reg.SetState(w1.PID, StateRunning)
	_ = reg.SetState(w2.PID, StateRunning)

	lm.CompleteTask(w1.PID, 0, nil, "")
	lm.CompleteTask(w2.PID, 0, nil, "")

	if lm.PendingResults() != 2 {
		t.Errorf("PendingResults=%d, want 2", lm.PendingResults())
	}

	lm.WaitResult(w1.PID)
	if lm.PendingResults() != 1 {
		t.Errorf("PendingResults=%d, want 1", lm.PendingResults())
	}
}

func TestLifecycleManager_CollapseBranch(t *testing.T) {
	lm, reg, _ := setupLifecycleTest()

	queen, _ := reg.GetByName("queen")
	w1, _ := reg.GetByName("worker1")
	w2, _ := reg.GetByName("worker2")
	arch, _ := reg.GetByName("architect")

	// Set all to running.
	_ = reg.SetState(w1.PID, StateRunning)
	_ = reg.SetState(w2.PID, StateRunning)
	_ = reg.SetState(arch.PID, StateRunning)

	results, err := lm.CollapseBranch(queen.PID)
	if err != nil {
		t.Fatalf("CollapseBranch: %v", err)
	}

	if len(results) != 3 {
		t.Errorf("results=%d, want 3 (worker1 + worker2 + architect)", len(results))
	}

	// All children should be dead.
	for _, child := range reg.GetChildren(queen.PID) {
		if child.State != StateDead {
			t.Errorf("child PID %d State=%s, want dead", child.PID, child.State)
		}
	}
}

func TestLifecycleManager_CollapseBranch_WithCompletedChildren(t *testing.T) {
	lm, reg, _ := setupLifecycleTest()

	queen, _ := reg.GetByName("queen")
	w1, _ := reg.GetByName("worker1")

	// worker1 already completed.
	_ = reg.SetState(w1.PID, StateRunning)
	lm.CompleteTask(w1.PID, 0, []byte("w1 output"), "")

	// Others running.
	w2, _ := reg.GetByName("worker2")
	_ = reg.SetState(w2.PID, StateRunning)
	arch, _ := reg.GetByName("architect")
	_ = reg.SetState(arch.PID, StateRunning)

	results, err := lm.CollapseBranch(queen.PID)
	if err != nil {
		t.Fatalf("CollapseBranch: %v", err)
	}

	if len(results) != 3 {
		t.Errorf("results=%d, want 3", len(results))
	}

	// Check that w1's real result was collected.
	var foundW1 bool
	for _, r := range results {
		if r.PID == w1.PID && string(r.Output) == "w1 output" {
			foundW1 = true
		}
	}
	if !foundW1 {
		t.Error("w1 completed result should be in collapsed results")
	}
}

func TestLifecycleManager_CollapseBranch_Empty(t *testing.T) {
	lm, _, _ := setupLifecycleTest()

	// king has only queen, but let's test with a PID that has no children.
	results, err := lm.CollapseBranch(9999)
	if err != nil {
		t.Fatalf("CollapseBranch empty: %v", err)
	}
	if results != nil {
		t.Errorf("results should be nil for no children, got %d", len(results))
	}
}

func TestLifecycleManager_IsAlive(t *testing.T) {
	lm, reg, _ := setupLifecycleTest()

	w, _ := reg.GetByName("worker1")
	_ = reg.SetState(w.PID, StateRunning)

	if !lm.IsAlive(w.PID) {
		t.Error("running worker should be alive")
	}

	_ = reg.SetState(w.PID, StateDead)
	if lm.IsAlive(w.PID) {
		t.Error("dead worker should not be alive")
	}

	if lm.IsAlive(9999) {
		t.Error("nonexistent process should not be alive")
	}
}

func TestLifecycleManager_ActiveChildren(t *testing.T) {
	lm, reg, _ := setupLifecycleTest()

	queen, _ := reg.GetByName("queen")
	w1, _ := reg.GetByName("worker1")

	// All children are idle (alive).
	active := lm.ActiveChildren(queen.PID)
	if active != 3 {
		t.Errorf("ActiveChildren=%d, want 3", active)
	}

	// Kill one.
	_ = reg.SetState(w1.PID, StateDead)
	active = lm.ActiveChildren(queen.PID)
	if active != 2 {
		t.Errorf("ActiveChildren=%d, want 2 after killing one", active)
	}
}
