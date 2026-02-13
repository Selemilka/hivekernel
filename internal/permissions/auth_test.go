package permissions

import (
	"testing"

	"github.com/selemilka/hivekernel/internal/process"
)

func setupAuthTree(t *testing.T) (*process.Registry, *AuthProvider) {
	t.Helper()
	r := process.NewRegistry()

	// king(1,root) -> queen(2,root) -> [worker-alice(3,alice), worker-bob(4,bob)]
	// worker-alice(3) -> task(5,alice)
	r.Register(&process.Process{Name: "king", User: "root", Role: process.RoleKernel})
	r.Register(&process.Process{PPID: 1, Name: "queen", User: "root", Role: process.RoleDaemon})
	r.Register(&process.Process{PPID: 2, Name: "worker-alice", User: "alice", Role: process.RoleWorker})
	r.Register(&process.Process{PPID: 2, Name: "worker-bob", User: "bob", Role: process.RoleWorker})
	r.Register(&process.Process{PPID: 3, Name: "task", User: "alice", Role: process.RoleTask})

	return r, NewAuthProvider(r)
}

func TestAuthResolve(t *testing.T) {
	_, auth := setupAuthTree(t)

	id, err := auth.Resolve(3)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id.User != "alice" {
		t.Fatalf("expected user alice, got %s", id.User)
	}
	if id.Role != process.RoleWorker {
		t.Fatalf("expected role worker, got %s", id.Role)
	}
}

func TestAuthInheritanceKernelCanAssignAnyUser(t *testing.T) {
	_, auth := setupAuthTree(t)

	// Kernel (PID 1) can spawn child with any user.
	err := auth.ValidateInheritance(1, "newuser")
	if err != nil {
		t.Fatalf("kernel should be able to assign any user: %v", err)
	}
}

func TestAuthInheritanceMustMatchParent(t *testing.T) {
	_, auth := setupAuthTree(t)

	// worker-alice (PID 3, user=alice) tries to spawn child with user=bob.
	err := auth.ValidateInheritance(3, "bob")
	if err == nil {
		t.Fatal("non-kernel should not assign different user")
	}

	// worker-alice (PID 3, user=alice) spawns child with same user — OK.
	err = auth.ValidateInheritance(3, "alice")
	if err != nil {
		t.Fatalf("same user should be allowed: %v", err)
	}

	// Empty user means inherit — should be OK.
	err = auth.ValidateInheritance(3, "")
	if err != nil {
		t.Fatalf("empty user (inherit) should be allowed: %v", err)
	}
}

func TestAuthIsSameUser(t *testing.T) {
	_, auth := setupAuthTree(t)

	// worker-alice(3) and task(5) are both alice.
	same, err := auth.IsSameUser(3, 5)
	if err != nil {
		t.Fatalf("IsSameUser: %v", err)
	}
	if !same {
		t.Fatal("PID 3 and 5 should be same user (alice)")
	}

	// worker-alice(3) and worker-bob(4) are different.
	same, err = auth.IsSameUser(3, 4)
	if err != nil {
		t.Fatalf("IsSameUser: %v", err)
	}
	if same {
		t.Fatal("PID 3 and 4 should be different users")
	}
}

func TestAuthIsKernel(t *testing.T) {
	_, auth := setupAuthTree(t)

	if !auth.IsKernel(1) {
		t.Fatal("PID 1 should be kernel")
	}
	if auth.IsKernel(3) {
		t.Fatal("PID 3 should not be kernel")
	}
}
