package cluster

import (
	"testing"

	"github.com/selemilka/hivekernel/internal/process"
)

func setupMigrationTest() (*MigrationManager, *process.Registry, *NodeRegistry) {
	reg := process.NewRegistry()
	nodes := NewNodeRegistry()

	// Register VPS nodes.
	nodes.Register(&NodeInfo{ID: "vps1", MemoryMB: 2048, ProcessCount: 5})
	nodes.Register(&NodeInfo{ID: "vps2", MemoryMB: 2048, MemoryUsedMB: 1900, ProcessCount: 20})
	nodes.Register(&NodeInfo{ID: "vps3", MemoryMB: 4096, ProcessCount: 2})

	// Create a process tree: king -> queen -> leo -> [worker1, worker2]
	spawner := process.NewSpawner(reg)
	king, _ := spawner.SpawnKernel("king", "root", "vps1")

	queen, _ := spawner.Spawn(process.SpawnRequest{
		ParentPID:     king.PID,
		Name:          "queen@vps2",
		Role:          process.RoleDaemon,
		CognitiveTier: process.CogTactical,
		VPS:           "vps2",
		Limits:        process.ResourceLimits{MaxChildren: 10},
	})

	leo, _ := spawner.Spawn(process.SpawnRequest{
		ParentPID:     queen.PID,
		Name:          "leo",
		Role:          process.RoleAgent,
		CognitiveTier: process.CogTactical,
		User:          "leo",
		VPS:           "vps2",
		Limits:        process.ResourceLimits{MaxChildren: 10},
	})

	spawner.Spawn(process.SpawnRequest{
		ParentPID:     leo.PID,
		Name:          "frontend-lead",
		Role:          process.RoleLead,
		CognitiveTier: process.CogTactical,
		User:          "leo",
		VPS:           "vps2",
		Limits:        process.ResourceLimits{MaxChildren: 10},
	})

	spawner.Spawn(process.SpawnRequest{
		ParentPID:     leo.PID,
		Name:          "backend-lead",
		Role:          process.RoleLead,
		CognitiveTier: process.CogTactical,
		User:          "leo",
		VPS:           "vps2",
		Limits:        process.ResourceLimits{MaxChildren: 10},
	})

	mm := NewMigrationManager(reg, nodes)
	return mm, reg, nodes
}

func TestMigrationManager_PrepareMigration(t *testing.T) {
	mm, reg, _ := setupMigrationTest()

	// Find leo's PID.
	leo, _ := reg.GetByName("leo")

	mig, err := mm.PrepareMigration(leo.PID, "vps3")
	if err != nil {
		t.Fatalf("PrepareMigration: %v", err)
	}

	if mig.State != MigrationPending {
		t.Errorf("State=%s, want pending", mig.State)
	}
	if mig.SourceNode != "vps2" {
		t.Errorf("SourceNode=%q, want vps2", mig.SourceNode)
	}
	if mig.TargetNode != "vps3" {
		t.Errorf("TargetNode=%q, want vps3", mig.TargetNode)
	}

	// Should snapshot leo + 2 leads = 3 processes.
	if len(mig.Processes) != 3 {
		t.Errorf("Processes=%d, want 3 (leo + 2 leads)", len(mig.Processes))
	}
}

func TestMigrationManager_PrepareMigration_CannotMigrateKernel(t *testing.T) {
	mm, _, _ := setupMigrationTest()

	_, err := mm.PrepareMigration(1, "vps3") // PID 1 = king
	if err == nil {
		t.Error("should not allow migrating kernel")
	}
}

func TestMigrationManager_PrepareMigration_SameNode(t *testing.T) {
	mm, reg, _ := setupMigrationTest()

	leo, _ := reg.GetByName("leo")
	_, err := mm.PrepareMigration(leo.PID, "vps2") // already on vps2
	if err == nil {
		t.Error("should not allow migrating to same node")
	}
}

func TestMigrationManager_PrepareMigration_TargetOffline(t *testing.T) {
	mm, reg, nodes := setupMigrationTest()

	_ = nodes.SetStatus("vps3", NodeOffline)
	leo, _ := reg.GetByName("leo")

	_, err := mm.PrepareMigration(leo.PID, "vps3")
	if err == nil {
		t.Error("should not allow migrating to offline node")
	}
}

func TestMigrationManager_ExecuteMigration(t *testing.T) {
	mm, reg, _ := setupMigrationTest()

	leo, _ := reg.GetByName("leo")
	mig, _ := mm.PrepareMigration(leo.PID, "vps3")

	err := mm.ExecuteMigration(mig.ID)
	if err != nil {
		t.Fatalf("ExecuteMigration: %v", err)
	}

	// Verify migration state.
	mig, _ = mm.GetMigration(mig.ID)
	if mig.State != MigrationCompleted {
		t.Errorf("State=%s, want completed", mig.State)
	}

	// Verify all processes moved to vps3.
	leoProc, _ := reg.GetByName("leo")
	if leoProc.VPS != "vps3" {
		t.Errorf("leo VPS=%q, want vps3", leoProc.VPS)
	}

	frontend, _ := reg.GetByName("frontend-lead")
	if frontend.VPS != "vps3" {
		t.Errorf("frontend-lead VPS=%q, want vps3", frontend.VPS)
	}

	backend, _ := reg.GetByName("backend-lead")
	if backend.VPS != "vps3" {
		t.Errorf("backend-lead VPS=%q, want vps3", backend.VPS)
	}
}

func TestMigrationManager_ExecuteMigration_UpdatesNodeCounts(t *testing.T) {
	mm, reg, nodes := setupMigrationTest()

	leo, _ := reg.GetByName("leo")
	mig, _ := mm.PrepareMigration(leo.PID, "vps3")

	srcBefore, _ := nodes.Get("vps2")
	srcCountBefore := srcBefore.ProcessCount
	tgtBefore, _ := nodes.Get("vps3")
	tgtCountBefore := tgtBefore.ProcessCount

	_ = mm.ExecuteMigration(mig.ID)

	srcAfter, _ := nodes.Get("vps2")
	tgtAfter, _ := nodes.Get("vps3")

	if srcAfter.ProcessCount != srcCountBefore-3 {
		t.Errorf("source count=%d, want %d", srcAfter.ProcessCount, srcCountBefore-3)
	}
	if tgtAfter.ProcessCount != tgtCountBefore+3 {
		t.Errorf("target count=%d, want %d", tgtAfter.ProcessCount, tgtCountBefore+3)
	}
}

func TestMigrationManager_RollbackMigration(t *testing.T) {
	mm, reg, _ := setupMigrationTest()

	leo, _ := reg.GetByName("leo")
	mig, _ := mm.PrepareMigration(leo.PID, "vps3")

	// Start the migration manually.
	mm.mu.Lock()
	mig.State = MigrationInProgress
	mm.mu.Unlock()

	// Partially migrate (simulated).
	_ = reg.Update(leo.PID, func(p *process.Process) {
		p.VPS = "vps3"
	})

	// Rollback.
	err := mm.RollbackMigration(mig.ID)
	if err != nil {
		t.Fatalf("RollbackMigration: %v", err)
	}

	mig, _ = mm.GetMigration(mig.ID)
	if mig.State != MigrationRolledBack {
		t.Errorf("State=%s, want rolled_back", mig.State)
	}

	leoProc, _ := reg.GetByName("leo")
	if leoProc.VPS != "vps2" {
		t.Errorf("leo VPS=%q, want vps2 (rolled back)", leoProc.VPS)
	}
}

func TestMigrationManager_RollbackMigration_InvalidState(t *testing.T) {
	mm, reg, _ := setupMigrationTest()

	leo, _ := reg.GetByName("leo")
	mig, _ := mm.PrepareMigration(leo.PID, "vps3")

	// Cannot rollback a pending migration.
	err := mm.RollbackMigration(mig.ID)
	if err == nil {
		t.Error("should not allow rolling back pending migration")
	}
}

func TestMigrationManager_ExecuteMigration_AlreadyRunning(t *testing.T) {
	mm, reg, _ := setupMigrationTest()

	leo, _ := reg.GetByName("leo")
	mig, _ := mm.PrepareMigration(leo.PID, "vps3")

	_ = mm.ExecuteMigration(mig.ID)

	// Try executing again.
	err := mm.ExecuteMigration(mig.ID)
	if err == nil {
		t.Error("should not allow executing completed migration")
	}
}

func TestMigrationManager_ListAndActive(t *testing.T) {
	mm, reg, _ := setupMigrationTest()

	leo, _ := reg.GetByName("leo")
	mm.PrepareMigration(leo.PID, "vps3")

	all := mm.ListMigrations()
	if len(all) != 1 {
		t.Errorf("ListMigrations=%d, want 1", len(all))
	}

	active := mm.ActiveMigrations()
	if len(active) != 1 {
		t.Errorf("ActiveMigrations=%d, want 1", len(active))
	}

	// Execute the migration.
	mm.ExecuteMigration(all[0].ID)

	active = mm.ActiveMigrations()
	if len(active) != 0 {
		t.Errorf("ActiveMigrations=%d after completion, want 0", len(active))
	}
}

func TestSnapshotProcess(t *testing.T) {
	p := &process.Process{
		PID:           42,
		PPID:          10,
		User:          "leo",
		Name:          "test-agent",
		Role:          process.RoleAgent,
		CognitiveTier: process.CogStrategic,
		Model:         "opus",
		State:         process.StateRunning,
		TokensConsumed: 5000,
		Limits: process.ResourceLimits{
			MaxChildren:    10,
			MaxTokensTotal: 100000,
		},
	}

	snap := SnapshotProcess(p)
	if snap.PID != 42 {
		t.Errorf("PID=%d, want 42", snap.PID)
	}
	if snap.User != "leo" {
		t.Errorf("User=%q, want leo", snap.User)
	}
	if snap.TokensConsumed != 5000 {
		t.Errorf("TokensConsumed=%d, want 5000", snap.TokensConsumed)
	}
	if snap.Limits.MaxChildren != 10 {
		t.Errorf("MaxChildren=%d, want 10", snap.Limits.MaxChildren)
	}
}

func TestMigrationManager_SourceDrainingRestored(t *testing.T) {
	mm, reg, nodes := setupMigrationTest()

	leo, _ := reg.GetByName("leo")
	mig, _ := mm.PrepareMigration(leo.PID, "vps3")

	// Before migration, vps2 is online.
	src, _ := nodes.Get("vps2")
	if src.Status != NodeOnline {
		t.Fatalf("vps2 status=%s, want online before migration", src.Status)
	}

	_ = mm.ExecuteMigration(mig.ID)

	// After migration, vps2 should be back to online (draining resolved).
	src, _ = nodes.Get("vps2")
	if src.Status != NodeOnline {
		t.Errorf("vps2 status=%s, want online after migration completes", src.Status)
	}
}
