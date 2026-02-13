package ipc

import (
	"strings"
	"testing"

	"github.com/selemilka/hivekernel/internal/process"
)

func setupSMTree(t *testing.T) (*process.Registry, *SharedMemory) {
	t.Helper()
	r := process.NewRegistry()

	// king(1) -> queen(2) -> [worker-alice(3), worker-bob(4)]
	// worker-alice(3) -> task(5)
	r.Register(&process.Process{Name: "king", User: "root", Role: process.RoleKernel})
	r.Register(&process.Process{PPID: 1, Name: "queen", User: "root", Role: process.RoleDaemon})
	r.Register(&process.Process{PPID: 2, Name: "worker-alice", User: "alice", Role: process.RoleWorker})
	r.Register(&process.Process{PPID: 2, Name: "worker-bob", User: "bob", Role: process.RoleWorker})
	r.Register(&process.Process{PPID: 3, Name: "task-alice", User: "alice", Role: process.RoleTask})

	return r, NewSharedMemory(r)
}

func TestSharedMemoryGlobal(t *testing.T) {
	_, sm := setupSMTree(t)

	id, err := sm.Store(3, "design.md", []byte("architecture"), "text/markdown", VisGlobal)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Everyone can read global artifacts.
	for _, pid := range []process.PID{1, 2, 3, 4, 5} {
		art, err := sm.Get(pid, "design.md")
		if err != nil {
			t.Fatalf("PID %d can't read global artifact: %v", pid, err)
		}
		if art.ID != id {
			t.Fatalf("wrong artifact ID")
		}
	}
}

func TestSharedMemoryPrivate(t *testing.T) {
	_, sm := setupSMTree(t)

	sm.Store(3, "secret.txt", []byte("private data"), "text/plain", VisPrivate)

	// Owner can read.
	_, err := sm.Get(3, "secret.txt")
	if err != nil {
		t.Fatalf("owner can't read own private artifact: %v", err)
	}

	// Others cannot.
	_, err = sm.Get(4, "secret.txt")
	if err == nil {
		t.Fatal("other user should not read private artifact")
	}
	_, err = sm.Get(1, "secret.txt")
	if err == nil {
		t.Fatal("king should not read private artifact")
	}
}

func TestSharedMemoryUserVisibility(t *testing.T) {
	_, sm := setupSMTree(t)

	// worker-alice (PID 3, user=alice) stores with user visibility.
	sm.Store(3, "alice-data.txt", []byte("alice stuff"), "text/plain", VisUser)

	// task-alice (PID 5, user=alice) can read.
	_, err := sm.Get(5, "alice-data.txt")
	if err != nil {
		t.Fatalf("same-user should read: %v", err)
	}

	// worker-bob (PID 4, user=bob) cannot read.
	_, err = sm.Get(4, "alice-data.txt")
	if err == nil {
		t.Fatal("different user should not read user-visibility artifact")
	}
}

func TestSharedMemorySubtreeVisibility(t *testing.T) {
	_, sm := setupSMTree(t)

	// worker-alice (PID 3) stores with subtree visibility.
	sm.Store(3, "subtree.txt", []byte("for my children"), "text/plain", VisSubtree)

	// task-alice (PID 5, child of 3) can read.
	_, err := sm.Get(5, "subtree.txt")
	if err != nil {
		t.Fatalf("descendant should read subtree artifact: %v", err)
	}

	// worker-bob (PID 4, not descendant) cannot.
	_, err = sm.Get(4, "subtree.txt")
	if err == nil {
		t.Fatal("non-descendant should not read subtree artifact")
	}

	// queen (PID 2, parent, not descendant) cannot.
	_, err = sm.Get(2, "subtree.txt")
	if err == nil {
		t.Fatal("parent (non-descendant) should not read subtree artifact")
	}
}

func TestSharedMemoryList(t *testing.T) {
	_, sm := setupSMTree(t)

	sm.Store(3, "docs/readme.md", []byte("readme"), "text/markdown", VisGlobal)
	sm.Store(3, "docs/api.md", []byte("api"), "text/markdown", VisGlobal)
	sm.Store(3, "config.yaml", []byte("config"), "text/yaml", VisGlobal)

	// List with prefix "docs/"
	arts := sm.List(1, "docs/")
	if len(arts) != 2 {
		t.Fatalf("expected 2 artifacts with prefix docs/, got %d", len(arts))
	}
}

func TestSharedMemoryDelete(t *testing.T) {
	_, sm := setupSMTree(t)

	sm.Store(3, "temp.txt", []byte("temp"), "text/plain", VisGlobal)

	// Non-owner, non-kernel can't delete.
	err := sm.Delete(4, "temp.txt")
	if err == nil {
		t.Fatal("non-owner should not delete")
	}

	// Owner can delete.
	err = sm.Delete(3, "temp.txt")
	if err != nil {
		t.Fatalf("owner should delete: %v", err)
	}

	// Verify gone.
	_, err = sm.Get(3, "temp.txt")
	if err == nil {
		t.Fatal("artifact should be gone after delete")
	}
}

func TestSharedMemoryKernelCanDelete(t *testing.T) {
	_, sm := setupSMTree(t)

	sm.Store(3, "cleanup.txt", []byte("data"), "text/plain", VisGlobal)

	// Kernel (PID 1) can delete anyone's artifact.
	err := sm.Delete(1, "cleanup.txt")
	if err != nil {
		t.Fatalf("kernel should be able to delete: %v", err)
	}
}

func TestSharedMemoryRequiresKey(t *testing.T) {
	_, sm := setupSMTree(t)

	_, err := sm.Store(3, "", []byte("data"), "text/plain", VisGlobal)
	if err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatal("expected error for empty key")
	}
}
