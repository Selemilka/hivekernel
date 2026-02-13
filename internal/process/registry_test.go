package process

import (
	"testing"
)

func TestRegistryBasicCRUD(t *testing.T) {
	r := NewRegistry()

	// Register a process.
	p := &Process{Name: "test-agent", User: "alice", Role: RoleAgent, CognitiveTier: CogStrategic}
	pid, err := r.Register(p)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if pid != 1 {
		t.Fatalf("expected PID 1, got %d", pid)
	}
	if p.PID != 1 {
		t.Fatalf("expected process PID to be set to 1, got %d", p.PID)
	}

	// Get by PID.
	got, err := r.Get(1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "test-agent" {
		t.Fatalf("expected name test-agent, got %s", got.Name)
	}

	// Get by name.
	got, err = r.GetByName("test-agent")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got.PID != 1 {
		t.Fatalf("expected PID 1, got %d", got.PID)
	}

	// Get non-existent.
	_, err = r.Get(999)
	if err == nil {
		t.Fatal("expected error for non-existent PID")
	}

	// Update.
	err = r.SetState(1, StateRunning)
	if err != nil {
		t.Fatalf("SetState: %v", err)
	}
	got, _ = r.Get(1)
	if got.State != StateRunning {
		t.Fatalf("expected StateRunning, got %s", got.State)
	}

	// Remove.
	err = r.Remove(1)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	_, err = r.Get(1)
	if err == nil {
		t.Fatal("expected error after Remove")
	}
}

func TestRegistryParentChild(t *testing.T) {
	r := NewRegistry()

	parent := &Process{Name: "parent", User: "root", Role: RoleKernel}
	r.Register(parent)

	child1 := &Process{PPID: parent.PID, Name: "child1", User: "root", Role: RoleDaemon}
	r.Register(child1)

	child2 := &Process{PPID: parent.PID, Name: "child2", User: "root", Role: RoleDaemon}
	r.Register(child2)

	grandchild := &Process{PPID: child1.PID, Name: "grandchild", User: "root", Role: RoleWorker}
	r.Register(grandchild)

	// Direct children.
	children := r.GetChildren(parent.PID)
	if len(children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(children))
	}

	// Descendants.
	desc := r.GetDescendants(parent.PID)
	if len(desc) != 3 {
		t.Fatalf("expected 3 descendants, got %d", len(desc))
	}

	// Count children.
	if c := r.CountChildren(parent.PID); c != 2 {
		t.Fatalf("expected CountChildren=2, got %d", c)
	}
}

func TestRegistryNearestCommonAncestor(t *testing.T) {
	r := NewRegistry()

	// Build tree: 1 -> 2 -> 4, 1 -> 3 -> 5
	root := &Process{Name: "root"}
	r.Register(root) // PID 1

	a := &Process{PPID: root.PID, Name: "a"}
	r.Register(a) // PID 2

	b := &Process{PPID: root.PID, Name: "b"}
	r.Register(b) // PID 3

	aa := &Process{PPID: a.PID, Name: "aa"}
	r.Register(aa) // PID 4

	bb := &Process{PPID: b.PID, Name: "bb"}
	r.Register(bb) // PID 5

	nca, found := r.NearestCommonAncestor(aa.PID, bb.PID)
	if !found {
		t.Fatal("expected to find NCA")
	}
	if nca != root.PID {
		t.Fatalf("expected NCA = PID %d, got %d", root.PID, nca)
	}

	nca, found = r.NearestCommonAncestor(aa.PID, a.PID)
	if !found {
		t.Fatal("expected to find NCA")
	}
	if nca != a.PID {
		t.Fatalf("expected NCA = PID %d, got %d", a.PID, nca)
	}
}

func TestPIDAutoIncrement(t *testing.T) {
	r := NewRegistry()

	for i := 0; i < 5; i++ {
		p := &Process{Name: "p" + string(rune('0'+i))}
		pid, _ := r.Register(p)
		if pid != uint64(i+1) {
			t.Fatalf("expected PID %d, got %d", i+1, pid)
		}
	}

	list := r.List()
	if len(list) != 5 {
		t.Fatalf("expected 5 processes, got %d", len(list))
	}
}
