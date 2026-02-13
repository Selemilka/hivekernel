package permissions

import (
	"testing"

	"github.com/selemilka/hivekernel/internal/process"
)

func setupACLTree(t *testing.T) (*process.Registry, *ACL) {
	t.Helper()
	r := process.NewRegistry()

	r.Register(&process.Process{Name: "king", User: "root", Role: process.RoleKernel})
	r.Register(&process.Process{PPID: 1, Name: "queen", User: "root", Role: process.RoleDaemon})
	r.Register(&process.Process{PPID: 2, Name: "worker-alice", User: "alice", Role: process.RoleWorker})
	r.Register(&process.Process{PPID: 2, Name: "worker-bob", User: "bob", Role: process.RoleWorker})
	r.Register(&process.Process{PPID: 3, Name: "task-alice", User: "alice", Role: process.RoleTask})

	auth := NewAuthProvider(r)
	return r, NewACL(r, auth)
}

func TestACLKernelCanDoAnything(t *testing.T) {
	_, acl := setupACLTree(t)

	for _, action := range []Action{ActionSpawn, ActionKill, ActionSendMessage, ActionReadArtifact, ActionWriteArtifact} {
		if err := acl.Check(1, action); err != nil {
			t.Fatalf("kernel should be allowed %s: %v", action, err)
		}
	}
}

func TestACLDaemonPermissions(t *testing.T) {
	_, acl := setupACLTree(t)

	// Daemon can spawn, kill, send messages.
	for _, action := range []Action{ActionSpawn, ActionKill, ActionSendMessage} {
		if err := acl.Check(2, action); err != nil {
			t.Fatalf("daemon should be allowed %s: %v", action, err)
		}
	}
}

func TestACLWorkerCanSpawn(t *testing.T) {
	_, acl := setupACLTree(t)

	err := acl.Check(3, ActionSpawn)
	if err != nil {
		t.Fatalf("worker should be allowed to spawn: %v", err)
	}
}

func TestACLTaskCannotSpawn(t *testing.T) {
	_, acl := setupACLTree(t)

	err := acl.Check(5, ActionSpawn)
	if err == nil {
		t.Fatal("task should NOT be allowed to spawn")
	}
}

func TestACLTaskCanSendMessage(t *testing.T) {
	_, acl := setupACLTree(t)

	err := acl.Check(5, ActionSendMessage)
	if err != nil {
		t.Fatalf("task should be allowed to send messages: %v", err)
	}
}

func TestACLTaskCanReadArtifact(t *testing.T) {
	_, acl := setupACLTree(t)

	err := acl.Check(5, ActionReadArtifact)
	if err != nil {
		t.Fatalf("task should be allowed to read artifacts: %v", err)
	}
}

func TestACLCrossUserDenied(t *testing.T) {
	_, acl := setupACLTree(t)

	// worker-alice(3) accessing worker-bob(4) — different users.
	err := acl.CheckCrossUser(3, 4)
	if err == nil {
		t.Fatal("cross-user access should be denied")
	}
}

func TestACLCrossUserSameUserAllowed(t *testing.T) {
	_, acl := setupACLTree(t)

	// worker-alice(3) accessing task-alice(5) — same user.
	err := acl.CheckCrossUser(3, 5)
	if err != nil {
		t.Fatalf("same-user access should be allowed: %v", err)
	}
}

func TestACLCrossUserKernelAllowed(t *testing.T) {
	_, acl := setupACLTree(t)

	// kernel(1) accessing worker-alice(3) — kernel can access anyone.
	err := acl.CheckCrossUser(1, 3)
	if err != nil {
		t.Fatalf("kernel should be allowed cross-user access: %v", err)
	}
}

func TestACLCustomRule(t *testing.T) {
	_, acl := setupACLTree(t)

	// Task can't write artifacts by default.
	err := acl.Check(5, ActionWriteArtifact)
	if err == nil {
		t.Fatal("task should not be able to write artifacts by default")
	}

	// Add custom rule to allow.
	acl.AddRule(ACLRule{Role: process.RoleTask, Action: ActionWriteArtifact, Allow: true})

	err = acl.Check(5, ActionWriteArtifact)
	if err != nil {
		t.Fatalf("task should be allowed to write after custom rule: %v", err)
	}
}
