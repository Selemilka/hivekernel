package process

import (
	"strings"
	"testing"
)

func TestSpawnKernel(t *testing.T) {
	r := NewRegistry()
	s := NewSpawner(r)

	proc, err := s.SpawnKernel("king", "root", "vps1")
	if err != nil {
		t.Fatalf("SpawnKernel: %v", err)
	}
	if proc.PID != 1 {
		t.Fatalf("expected PID 1, got %d", proc.PID)
	}
	if proc.Role != RoleKernel {
		t.Fatalf("expected RoleKernel, got %s", proc.Role)
	}
	if proc.State != StateRunning {
		t.Fatalf("expected StateRunning, got %s", proc.State)
	}
}

func TestSpawnChildBasic(t *testing.T) {
	r := NewRegistry()
	s := NewSpawner(r)

	king, _ := s.SpawnKernel("king", "root", "vps1")

	// Spawn a daemon under king (tactical <= strategic: OK).
	queen, err := s.Spawn(SpawnRequest{
		ParentPID:     king.PID,
		Name:          "queen@vps1",
		Role:          RoleDaemon,
		CognitiveTier: CogTactical,
	})
	if err != nil {
		t.Fatalf("Spawn queen: %v", err)
	}
	if queen.PPID != king.PID {
		t.Fatalf("expected PPID %d, got %d", king.PID, queen.PPID)
	}
	// Should inherit user and VPS from parent.
	if queen.User != "root" {
		t.Fatalf("expected user root, got %s", queen.User)
	}
	if queen.VPS != "vps1" {
		t.Fatalf("expected VPS vps1, got %s", queen.VPS)
	}
	// Default model for tactical.
	if queen.Model != "sonnet" {
		t.Fatalf("expected model sonnet, got %s", queen.Model)
	}
}

func TestSpawnRejectsCogTierViolation(t *testing.T) {
	r := NewRegistry()
	s := NewSpawner(r)

	// King is strategic.
	king, _ := s.SpawnKernel("king", "root", "vps1")

	// Spawn a tactical queen.
	queen, _ := s.Spawn(SpawnRequest{
		ParentPID:     king.PID,
		Name:          "queen",
		Role:          RoleDaemon,
		CognitiveTier: CogTactical,
	})

	// Try to spawn a strategic child under tactical queen — must fail.
	_, err := s.Spawn(SpawnRequest{
		ParentPID:     queen.PID,
		Name:          "bad-child",
		Role:          RoleLead,
		CognitiveTier: CogStrategic,
	})
	if err == nil {
		t.Fatal("expected error: strategic child under tactical parent")
	}
	if !strings.Contains(err.Error(), "cognitive tier") {
		t.Fatalf("expected cognitive tier error, got: %v", err)
	}
}

func TestSpawnRejectsMaxChildren(t *testing.T) {
	r := NewRegistry()
	s := NewSpawner(r)

	king, _ := s.SpawnKernel("king", "root", "vps1")

	// Set max_children to 2 on queen.
	queen, _ := s.Spawn(SpawnRequest{
		ParentPID:     king.PID,
		Name:          "queen",
		Role:          RoleDaemon,
		CognitiveTier: CogTactical,
		Limits:        ResourceLimits{MaxChildren: 2},
	})

	// Spawn 2 workers — OK.
	for i := 0; i < 2; i++ {
		_, err := s.Spawn(SpawnRequest{
			ParentPID:     queen.PID,
			Name:          "worker-" + string(rune('a'+i)),
			Role:          RoleWorker,
			CognitiveTier: CogOperational,
		})
		if err != nil {
			t.Fatalf("spawn worker %d: %v", i, err)
		}
	}

	// Third worker — must fail.
	_, err := s.Spawn(SpawnRequest{
		ParentPID:     queen.PID,
		Name:          "worker-overflow",
		Role:          RoleWorker,
		CognitiveTier: CogOperational,
	})
	if err == nil {
		t.Fatal("expected error: max_children exceeded")
	}
}

func TestSpawnRejectsStrategicTask(t *testing.T) {
	r := NewRegistry()
	s := NewSpawner(r)

	king, _ := s.SpawnKernel("king", "root", "vps1")

	// Task + strategic is forbidden.
	_, err := s.Spawn(SpawnRequest{
		ParentPID:     king.PID,
		Name:          "bad-task",
		Role:          RoleTask,
		CognitiveTier: CogStrategic,
	})
	if err == nil {
		t.Fatal("expected error: task cannot be strategic")
	}
}

func TestSpawnRejectsDeadParent(t *testing.T) {
	r := NewRegistry()
	s := NewSpawner(r)

	king, _ := s.SpawnKernel("king", "root", "vps1")
	worker, _ := s.Spawn(SpawnRequest{
		ParentPID: king.PID, Name: "worker",
		Role: RoleWorker, CognitiveTier: CogOperational,
	})

	// Kill the worker (zombie state).
	_ = r.SetState(worker.PID, StateZombie)
	_, err := s.Spawn(SpawnRequest{
		ParentPID: worker.PID, Name: "child-of-zombie",
		Role: RoleWorker, CognitiveTier: CogOperational,
	})
	if err == nil {
		t.Fatal("expected error: zombie parent cannot spawn")
	}

	// Dead state.
	_ = r.SetState(worker.PID, StateDead)
	_, err = s.Spawn(SpawnRequest{
		ParentPID: worker.PID, Name: "child-of-dead",
		Role: RoleWorker, CognitiveTier: CogOperational,
	})
	if err == nil {
		t.Fatal("expected error: dead parent cannot spawn")
	}
}

func TestSpawnRequiresName(t *testing.T) {
	r := NewRegistry()
	s := NewSpawner(r)

	king, _ := s.SpawnKernel("king", "root", "vps1")

	_, err := s.Spawn(SpawnRequest{
		ParentPID:     king.PID,
		Name:          "",
		Role:          RoleWorker,
		CognitiveTier: CogOperational,
	})
	if err == nil {
		t.Fatal("expected error: name required")
	}
}
