package permissions

import (
	"testing"

	"github.com/selemilka/hivekernel/internal/process"
)

func setupCapTree(t *testing.T) (*process.Registry, *CapabilityChecker) {
	t.Helper()
	r := process.NewRegistry()

	r.Register(&process.Process{Name: "king", User: "root", Role: process.RoleKernel})
	r.Register(&process.Process{PPID: 1, Name: "queen", User: "root", Role: process.RoleDaemon})
	r.Register(&process.Process{PPID: 2, Name: "lead", User: "leo", Role: process.RoleLead})
	r.Register(&process.Process{PPID: 3, Name: "worker", User: "leo", Role: process.RoleWorker})
	r.Register(&process.Process{PPID: 4, Name: "task", User: "leo", Role: process.RoleTask})

	return r, NewCapabilityChecker(r)
}

func TestCapabilityKernelHasAll(t *testing.T) {
	_, cc := setupCapTree(t)

	for _, cap := range []Capability{CapSpawnChildren, CapKillProcesses, CapShellExec, CapManageTree} {
		has, err := cc.HasCapability(1, cap)
		if err != nil {
			t.Fatalf("HasCapability: %v", err)
		}
		if !has {
			t.Fatalf("kernel should have %s", cap)
		}
	}
}

func TestCapabilityTaskNoSpawn(t *testing.T) {
	_, cc := setupCapTree(t)

	has, err := cc.HasCapability(5, CapSpawnChildren)
	if err != nil {
		t.Fatalf("HasCapability: %v", err)
	}
	if has {
		t.Fatal("task should NOT have spawn_children capability")
	}
}

func TestCapabilityTaskNoShellExec(t *testing.T) {
	_, cc := setupCapTree(t)

	has, err := cc.HasCapability(5, CapShellExec)
	if err != nil {
		t.Fatalf("HasCapability: %v", err)
	}
	if has {
		t.Fatal("task should NOT have shell_exec capability")
	}
}

func TestCapabilityWorkerCanSpawn(t *testing.T) {
	_, cc := setupCapTree(t)

	has, err := cc.HasCapability(4, CapSpawnChildren)
	if err != nil {
		t.Fatalf("HasCapability: %v", err)
	}
	if !has {
		t.Fatal("worker should have spawn_children capability")
	}
}

func TestCapabilityLeadCanKill(t *testing.T) {
	_, cc := setupCapTree(t)

	has, err := cc.HasCapability(3, CapKillProcesses)
	if err != nil {
		t.Fatalf("HasCapability: %v", err)
	}
	if !has {
		t.Fatal("lead should have kill_processes capability")
	}
}

func TestCapabilityRequire(t *testing.T) {
	_, cc := setupCapTree(t)

	// Task requires shell_exec — should fail.
	err := cc.RequireCapability(5, CapShellExec)
	if err == nil {
		t.Fatal("expected error for task requiring shell_exec")
	}

	// Kernel requires shell_exec — should succeed.
	err = cc.RequireCapability(1, CapShellExec)
	if err != nil {
		t.Fatalf("kernel should have shell_exec: %v", err)
	}
}

func TestCapabilityGrantOverride(t *testing.T) {
	_, cc := setupCapTree(t)

	// Task doesn't have file_write by default.
	has, _ := cc.HasCapability(5, CapFileWrite)
	if has {
		t.Fatal("task should not have file_write by default")
	}

	// Grant file_write to this specific task.
	cc.Grant(5, CapFileWrite)

	has, _ = cc.HasCapability(5, CapFileWrite)
	if !has {
		t.Fatal("task should have file_write after grant")
	}
}

func TestCapabilityRevokeOverride(t *testing.T) {
	_, cc := setupCapTree(t)

	// Worker has network_access by default.
	has, _ := cc.HasCapability(4, CapNetworkAccess)
	if !has {
		t.Fatal("worker should have network_access by default")
	}

	// Revoke it.
	cc.Revoke(4, CapNetworkAccess)

	has, _ = cc.HasCapability(4, CapNetworkAccess)
	if has {
		t.Fatal("worker should NOT have network_access after revoke")
	}
}

func TestCapabilityValidateTools(t *testing.T) {
	_, cc := setupCapTree(t)

	// Task with shell_exec tool — should fail.
	err := cc.ValidateTools(5, []string{"shell_exec"})
	if err == nil {
		t.Fatal("task should not be allowed shell_exec tool")
	}

	// Worker with file_write tool — should be OK.
	err = cc.ValidateTools(4, []string{"file_write"})
	if err != nil {
		t.Fatalf("worker should be allowed file_write tool: %v", err)
	}

	// Kernel with anything — should be OK.
	err = cc.ValidateTools(1, []string{"shell_exec", "file_write", "bash"})
	if err != nil {
		t.Fatalf("kernel should be allowed any tool: %v", err)
	}
}

func TestCapabilityListCapabilities(t *testing.T) {
	caps := ListCapabilities(process.RoleTask)
	if len(caps) == 0 {
		t.Fatal("task should have some capabilities")
	}

	found := false
	for _, c := range caps {
		if c == CapEscalate {
			found = true
		}
		if c == CapSpawnChildren {
			t.Fatal("task should not have spawn_children in list")
		}
	}
	if !found {
		t.Fatal("task should have escalate capability")
	}
}
