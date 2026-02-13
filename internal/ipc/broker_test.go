package ipc

import (
	"testing"

	"github.com/selemilka/hivekernel/internal/process"
)

// setupBrokerTree creates: king(1) -> queen(2) -> [worker-a(3), worker-b(4)]
// worker-a(3) -> task(5)
func setupBrokerTree(t *testing.T) (*process.Registry, *Broker) {
	t.Helper()
	r := process.NewRegistry()

	king := &process.Process{Name: "king", User: "root", Role: process.RoleKernel, State: process.StateRunning}
	r.Register(king) // 1

	queen := &process.Process{PPID: 1, Name: "queen", User: "root", Role: process.RoleDaemon, State: process.StateRunning}
	r.Register(queen) // 2

	wa := &process.Process{PPID: 2, Name: "worker-a", User: "root", Role: process.RoleWorker, State: process.StateRunning}
	r.Register(wa) // 3

	wb := &process.Process{PPID: 2, Name: "worker-b", User: "root", Role: process.RoleWorker, State: process.StateRunning}
	r.Register(wb) // 4

	task := &process.Process{PPID: 3, Name: "task", User: "root", Role: process.RoleTask, State: process.StateRunning}
	r.Register(task) // 5

	b := NewBroker(r, 0.1)
	return r, b
}

func TestBrokerParentChild(t *testing.T) {
	_, b := setupBrokerTree(t)

	// Queen (2) -> worker-a (3): direct, should work.
	err := b.Route(&Message{FromPID: 2, ToPID: 3, Type: "task", Priority: PriorityNormal})
	if err != nil {
		t.Fatalf("parent->child route failed: %v", err)
	}

	// Check worker-a's inbox has the message.
	if b.InboxLen(3) != 1 {
		t.Fatalf("expected 1 message in inbox, got %d", b.InboxLen(3))
	}
}

func TestBrokerChildToParent(t *testing.T) {
	_, b := setupBrokerTree(t)

	// Worker-a (3) -> queen (2): direct.
	err := b.Route(&Message{FromPID: 3, ToPID: 2, Type: "result", Priority: PriorityNormal})
	if err != nil {
		t.Fatalf("child->parent route failed: %v", err)
	}
	if b.InboxLen(2) != 1 {
		t.Fatalf("expected 1 message in queen inbox")
	}
}

func TestBrokerSiblings(t *testing.T) {
	_, b := setupBrokerTree(t)

	// Worker-a (3) -> worker-b (4): siblings, parent (queen) should see.
	err := b.Route(&Message{FromPID: 3, ToPID: 4, Type: "sync", Priority: PriorityNormal})
	if err != nil {
		t.Fatalf("sibling route failed: %v", err)
	}

	// Worker-b should have the message.
	if b.InboxLen(4) != 1 {
		t.Fatalf("expected 1 message in worker-b inbox, got %d", b.InboxLen(4))
	}

	// Queen (parent) should have a copy.
	if b.InboxLen(2) != 1 {
		t.Fatalf("expected 1 message in queen inbox (sibling traffic copy), got %d", b.InboxLen(2))
	}
}

func TestBrokerTaskCanOnlySendToParent(t *testing.T) {
	_, b := setupBrokerTree(t)

	// Task (5) -> worker-a (3, its parent): OK.
	err := b.Route(&Message{FromPID: 5, ToPID: 3, Type: "result"})
	if err != nil {
		t.Fatalf("task->parent should work: %v", err)
	}

	// Task (5) -> queen (2, not its parent): REJECTED.
	err = b.Route(&Message{FromPID: 5, ToPID: 2, Type: "hack"})
	if err == nil {
		t.Fatal("task should not be able to send to non-parent")
	}

	// Task (5) -> worker-b (4, sibling of parent): REJECTED.
	err = b.Route(&Message{FromPID: 5, ToPID: 4, Type: "hack"})
	if err == nil {
		t.Fatal("task should not be able to send to uncle")
	}
}

func TestBrokerNamedQueue(t *testing.T) {
	_, b := setupBrokerTree(t)

	// Send to named queue.
	err := b.Route(&Message{FromPID: 3, ToQueue: "results", Type: "data", Payload: []byte("hello")})
	if err != nil {
		t.Fatalf("named queue route failed: %v", err)
	}

	// Read from queue.
	q := b.GetNamedQueue("results")
	msg := q.Pop()
	if msg == nil {
		t.Fatal("expected message in named queue")
	}
	if string(msg.Payload) != "hello" {
		t.Fatalf("expected payload 'hello', got %q", string(msg.Payload))
	}
}

func TestBrokerCrossBranch(t *testing.T) {
	r := process.NewRegistry()

	king := &process.Process{Name: "king", User: "root", Role: process.RoleKernel, State: process.StateRunning}
	r.Register(king) // 1

	// Two branches: queen-a(2)->worker-a(4), queen-b(3)->worker-b(5)
	qa := &process.Process{PPID: 1, Name: "queen-a", User: "root", Role: process.RoleDaemon, State: process.StateRunning}
	r.Register(qa) // 2

	qb := &process.Process{PPID: 1, Name: "queen-b", User: "root", Role: process.RoleDaemon, State: process.StateRunning}
	r.Register(qb) // 3

	wa := &process.Process{PPID: 2, Name: "worker-a", User: "alice", Role: process.RoleWorker, State: process.StateRunning}
	r.Register(wa) // 4

	wb := &process.Process{PPID: 3, Name: "worker-b", User: "bob", Role: process.RoleWorker, State: process.StateRunning}
	r.Register(wb) // 5

	b := NewBroker(r, 0.1)

	// Worker-a (4) -> worker-b (5): cross-branch, through NCA (king).
	err := b.Route(&Message{FromPID: 4, ToPID: 5, Type: "cross"})
	if err != nil {
		t.Fatalf("cross-branch route failed: %v", err)
	}
	if b.InboxLen(5) != 1 {
		t.Fatalf("expected 1 message in worker-b inbox")
	}
}
