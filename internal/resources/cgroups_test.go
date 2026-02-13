package resources

import (
	"testing"

	"github.com/selemilka/hivekernel/internal/process"
)

func setupCGroupTest() (*CGroupManager, *process.Registry) {
	reg := process.NewRegistry()
	spawner := process.NewSpawner(reg)

	// king -> queen -> [worker1, worker2]
	king, _ := spawner.SpawnKernel("king", "root", "vps1")
	queen, _ := spawner.Spawn(process.SpawnRequest{
		ParentPID:     king.PID,
		Name:          "queen",
		Role:          process.RoleDaemon,
		CognitiveTier: process.CogTactical,
		Limits:        process.ResourceLimits{MaxChildren: 10},
	})
	spawner.Spawn(process.SpawnRequest{
		ParentPID:     queen.PID,
		Name:          "worker1",
		Role:          process.RoleWorker,
		CognitiveTier: process.CogTactical,
		Limits:        process.ResourceLimits{MaxChildren: 5},
	})
	spawner.Spawn(process.SpawnRequest{
		ParentPID:     queen.PID,
		Name:          "worker2",
		Role:          process.RoleWorker,
		CognitiveTier: process.CogTactical,
		Limits:        process.ResourceLimits{MaxChildren: 5},
	})

	mgr := NewCGroupManager(reg)
	return mgr, reg
}

func TestCGroupManager_Create(t *testing.T) {
	mgr, reg := setupCGroupTest()

	queen, _ := reg.GetByName("queen")
	g, err := mgr.Create("queen-branch", queen.PID, 100000, 10)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if g.Name != "queen-branch" {
		t.Errorf("Name=%q, want queen-branch", g.Name)
	}
	if g.RootPID != queen.PID {
		t.Errorf("RootPID=%d, want %d", g.RootPID, queen.PID)
	}
	// Should include queen + worker1 + worker2 = 3.
	if g.ProcessCount != 3 {
		t.Errorf("ProcessCount=%d, want 3", g.ProcessCount)
	}
	if len(g.Members()) != 3 {
		t.Errorf("Members=%d, want 3", len(g.Members()))
	}
}

func TestCGroupManager_Create_Duplicate(t *testing.T) {
	mgr, reg := setupCGroupTest()

	queen, _ := reg.GetByName("queen")
	mgr.Create("grp", queen.PID, 100000, 10)

	_, err := mgr.Create("grp", queen.PID, 100000, 10)
	if err == nil {
		t.Error("duplicate create should fail")
	}
}

func TestCGroupManager_Create_InvalidRoot(t *testing.T) {
	mgr, _ := setupCGroupTest()

	_, err := mgr.Create("grp", 9999, 100000, 10)
	if err == nil {
		t.Error("invalid root PID should fail")
	}
}

func TestCGroupManager_Delete(t *testing.T) {
	mgr, reg := setupCGroupTest()

	queen, _ := reg.GetByName("queen")
	mgr.Create("grp", queen.PID, 100000, 10)

	if err := mgr.Delete("grp"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := mgr.Get("grp"); err == nil {
		t.Error("Get after delete should fail")
	}

	// PID should no longer be in any group.
	if _, ok := mgr.GetByPID(queen.PID); ok {
		t.Error("queen should not be in any group after delete")
	}
}

func TestCGroupManager_GetByPID(t *testing.T) {
	mgr, reg := setupCGroupTest()

	queen, _ := reg.GetByName("queen")
	mgr.Create("grp", queen.PID, 100000, 10)

	g, ok := mgr.GetByPID(queen.PID)
	if !ok || g.Name != "grp" {
		t.Errorf("GetByPID: ok=%v, name=%q", ok, g.Name)
	}

	worker1, _ := reg.GetByName("worker1")
	g, ok = mgr.GetByPID(worker1.PID)
	if !ok || g.Name != "grp" {
		t.Errorf("worker1 should be in grp: ok=%v", ok)
	}

	king, _ := reg.GetByName("king")
	_, ok = mgr.GetByPID(king.PID)
	if ok {
		t.Error("king should not be in any group")
	}
}

func TestCGroupManager_AddProcess(t *testing.T) {
	mgr, reg := setupCGroupTest()

	queen, _ := reg.GetByName("queen")
	mgr.Create("grp", queen.PID, 100000, 10)

	// Add king to the group.
	king, _ := reg.GetByName("king")
	if err := mgr.AddProcess("grp", king.PID); err != nil {
		t.Fatalf("AddProcess: %v", err)
	}

	g, _ := mgr.Get("grp")
	if g.ProcessCount != 4 {
		t.Errorf("ProcessCount=%d, want 4", g.ProcessCount)
	}
}

func TestCGroupManager_AddProcess_MaxReached(t *testing.T) {
	mgr, reg := setupCGroupTest()

	queen, _ := reg.GetByName("queen")
	// Create with max 3 (queen + 2 workers already fill it).
	mgr.Create("grp", queen.PID, 100000, 3)

	king, _ := reg.GetByName("king")
	err := mgr.AddProcess("grp", king.PID)
	if err == nil {
		t.Error("should fail: max processes reached")
	}
}

func TestCGroupManager_AddProcess_Idempotent(t *testing.T) {
	mgr, reg := setupCGroupTest()

	queen, _ := reg.GetByName("queen")
	mgr.Create("grp", queen.PID, 100000, 10)

	// Adding queen again should be idempotent.
	if err := mgr.AddProcess("grp", queen.PID); err != nil {
		t.Fatalf("idempotent add should not fail: %v", err)
	}

	g, _ := mgr.Get("grp")
	if g.ProcessCount != 3 {
		t.Errorf("ProcessCount=%d, want 3 (idempotent)", g.ProcessCount)
	}
}

func TestCGroupManager_RemoveProcess(t *testing.T) {
	mgr, reg := setupCGroupTest()

	queen, _ := reg.GetByName("queen")
	mgr.Create("grp", queen.PID, 100000, 10)

	worker1, _ := reg.GetByName("worker1")
	mgr.RemoveProcess(worker1.PID)

	g, _ := mgr.Get("grp")
	if g.ProcessCount != 2 {
		t.Errorf("ProcessCount=%d, want 2", g.ProcessCount)
	}

	_, ok := mgr.GetByPID(worker1.PID)
	if ok {
		t.Error("worker1 should not be in any group after remove")
	}
}

func TestCGroupManager_ConsumeTokens(t *testing.T) {
	mgr, reg := setupCGroupTest()

	queen, _ := reg.GetByName("queen")
	mgr.Create("grp", queen.PID, 10000, 10)

	worker1, _ := reg.GetByName("worker1")
	if err := mgr.ConsumeTokens(worker1.PID, 5000); err != nil {
		t.Fatalf("ConsumeTokens: %v", err)
	}

	g, _ := mgr.Get("grp")
	if g.TokensConsumed != 5000 {
		t.Errorf("TokensConsumed=%d, want 5000", g.TokensConsumed)
	}
	if g.Remaining() != 5000 {
		t.Errorf("Remaining=%d, want 5000", g.Remaining())
	}
}

func TestCGroupManager_ConsumeTokens_ExceedsLimit(t *testing.T) {
	mgr, reg := setupCGroupTest()

	queen, _ := reg.GetByName("queen")
	mgr.Create("grp", queen.PID, 10000, 10)

	worker1, _ := reg.GetByName("worker1")
	_ = mgr.ConsumeTokens(worker1.PID, 8000)

	// Try to consume more than remaining.
	worker2, _ := reg.GetByName("worker2")
	err := mgr.ConsumeTokens(worker2.PID, 5000)
	if err == nil {
		t.Error("should fail: exceeds group token limit")
	}

	// Verify no partial consumption.
	g, _ := mgr.Get("grp")
	if g.TokensConsumed != 8000 {
		t.Errorf("TokensConsumed=%d, want 8000 (unchanged)", g.TokensConsumed)
	}
}

func TestCGroupManager_ConsumeTokens_NotInGroup(t *testing.T) {
	mgr, _ := setupCGroupTest()

	// Process not in any group — should succeed silently.
	err := mgr.ConsumeTokens(9999, 1000)
	if err != nil {
		t.Errorf("consume for ungrouped PID should succeed: %v", err)
	}
}

func TestCGroupManager_CheckSpawnAllowed(t *testing.T) {
	mgr, reg := setupCGroupTest()

	queen, _ := reg.GetByName("queen")
	mgr.Create("grp", queen.PID, 100000, 3) // already at max (3 members)

	err := mgr.CheckSpawnAllowed(queen.PID)
	if err == nil {
		t.Error("should deny spawn: group at max processes")
	}

	// Increase limit — should allow.
	g, _ := mgr.Get("grp")
	g.MaxProcesses = 10

	err = mgr.CheckSpawnAllowed(queen.PID)
	if err != nil {
		t.Errorf("should allow spawn with higher limit: %v", err)
	}
}

func TestCGroupManager_CheckSpawnAllowed_NotInGroup(t *testing.T) {
	mgr, _ := setupCGroupTest()

	// Process not in any group — always allowed.
	err := mgr.CheckSpawnAllowed(9999)
	if err != nil {
		t.Errorf("ungrouped process should always be allowed to spawn: %v", err)
	}
}

func TestCGroupManager_List(t *testing.T) {
	mgr, reg := setupCGroupTest()

	queen, _ := reg.GetByName("queen")
	mgr.Create("grp1", queen.PID, 100000, 10)

	king, _ := reg.GetByName("king")
	mgr.Create("grp2", king.PID, 200000, 20)

	list := mgr.List()
	if len(list) != 2 {
		t.Errorf("List len=%d, want 2", len(list))
	}
}

func TestCGroupManager_GetGroupUsage(t *testing.T) {
	mgr, reg := setupCGroupTest()

	queen, _ := reg.GetByName("queen")
	mgr.Create("grp", queen.PID, 10000, 10)

	worker1, _ := reg.GetByName("worker1")
	mgr.ConsumeTokens(worker1.PID, 2500)

	usage, err := mgr.GetGroupUsage("grp")
	if err != nil {
		t.Fatalf("GetGroupUsage: %v", err)
	}

	if usage.TokensConsumed != 2500 {
		t.Errorf("TokensConsumed=%d, want 2500", usage.TokensConsumed)
	}
	if usage.TokensMax != 10000 {
		t.Errorf("TokensMax=%d, want 10000", usage.TokensMax)
	}
	if usage.ProcessCount != 3 {
		t.Errorf("ProcessCount=%d, want 3", usage.ProcessCount)
	}
	if usage.UsagePercent != 25.0 {
		t.Errorf("UsagePercent=%f, want 25.0", usage.UsagePercent)
	}
}

func TestCGroup_Remaining(t *testing.T) {
	g := &CGroup{MaxTokensTotal: 10000, TokensConsumed: 7000}
	if g.Remaining() != 3000 {
		t.Errorf("Remaining=%d, want 3000", g.Remaining())
	}

	g.TokensConsumed = 15000 // over budget
	if g.Remaining() != 0 {
		t.Errorf("Remaining=%d, want 0 (over budget)", g.Remaining())
	}

	// Unlimited (0 max).
	g2 := &CGroup{MaxTokensTotal: 0, TokensConsumed: 5000}
	if g2.Remaining() != 0 {
		t.Errorf("Remaining=%d for unlimited, want 0", g2.Remaining())
	}
}

func TestCGroup_UsagePercent(t *testing.T) {
	g := &CGroup{MaxTokensTotal: 10000, TokensConsumed: 5000}
	if g.UsagePercent() != 50.0 {
		t.Errorf("UsagePercent=%f, want 50.0", g.UsagePercent())
	}

	g2 := &CGroup{MaxTokensTotal: 0}
	if g2.UsagePercent() != 0 {
		t.Errorf("UsagePercent=%f for unlimited, want 0", g2.UsagePercent())
	}
}
