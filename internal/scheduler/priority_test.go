package scheduler

import (
	"testing"
	"time"

	"github.com/selemilka/hivekernel/internal/process"
)

func TestBasePriorityFromTier(t *testing.T) {
	strat := BasePriorityFromTier(process.CogStrategic)
	tact := BasePriorityFromTier(process.CogTactical)
	oper := BasePriorityFromTier(process.CogOperational)

	if strat <= tact {
		t.Errorf("strategic (%d) should be > tactical (%d)", strat, tact)
	}
	if tact <= oper {
		t.Errorf("tactical (%d) should be > operational (%d)", tact, oper)
	}
}

func TestBasePriorityFromRole(t *testing.T) {
	kernel := BasePriorityFromRole(process.RoleKernel)
	daemon := BasePriorityFromRole(process.RoleDaemon)
	task := BasePriorityFromRole(process.RoleTask)

	if kernel <= daemon {
		t.Errorf("kernel (%d) should be > daemon (%d)", kernel, daemon)
	}
	if daemon <= task {
		t.Errorf("daemon (%d) should be > task (%d)", daemon, task)
	}
}

func TestComputeBasePriority(t *testing.T) {
	stratKernel := ComputeBasePriority(process.CogStrategic, process.RoleKernel)
	operTask := ComputeBasePriority(process.CogOperational, process.RoleTask)

	if stratKernel <= operTask {
		t.Errorf("strategic+kernel (%d) should be > operational+task (%d)", stratKernel, operTask)
	}
}

func TestTaskPriority_Effective(t *testing.T) {
	p := TaskPriority{
		Base:        100,
		EnqueuedAt:  time.Now().Add(-10 * time.Second),
		AgingFactor: 1.0,
	}

	eff := p.Effective()
	// Should be approximately 100 + 10*1.0 = 110.
	if eff < 109 || eff > 111 {
		t.Errorf("Effective=%f, expected ~110", eff)
	}
}

func TestTaskPriority_AgingIncreases(t *testing.T) {
	p := TaskPriority{
		Base:        50,
		EnqueuedAt:  time.Now(),
		AgingFactor: 0.5,
	}

	early := p.Effective()
	time.Sleep(10 * time.Millisecond)
	later := p.Effective()

	if later <= early {
		t.Errorf("priority should increase with age: early=%f, later=%f", early, later)
	}
}

func TestReadyQueue_PushPop(t *testing.T) {
	q := NewReadyQueue()

	q.Push(&TaskEntry{
		ID:   "low",
		Priority: TaskPriority{Base: 10, EnqueuedAt: time.Now(), AgingFactor: 0.1},
	})
	q.Push(&TaskEntry{
		ID:   "high",
		Priority: TaskPriority{Base: 100, EnqueuedAt: time.Now(), AgingFactor: 0.1},
	})
	q.Push(&TaskEntry{
		ID:   "mid",
		Priority: TaskPriority{Base: 50, EnqueuedAt: time.Now(), AgingFactor: 0.1},
	})

	if q.Len() != 3 {
		t.Errorf("Len=%d, want 3", q.Len())
	}

	// Should come out highest first.
	first := q.Pop()
	if first.ID != "high" {
		t.Errorf("first=%q, want high", first.ID)
	}

	second := q.Pop()
	if second.ID != "mid" {
		t.Errorf("second=%q, want mid", second.ID)
	}

	third := q.Pop()
	if third.ID != "low" {
		t.Errorf("third=%q, want low", third.ID)
	}
}

func TestReadyQueue_PopEmpty(t *testing.T) {
	q := NewReadyQueue()
	if q.Pop() != nil {
		t.Error("Pop on empty queue should return nil")
	}
}

func TestReadyQueue_Peek(t *testing.T) {
	q := NewReadyQueue()

	if q.Peek() != nil {
		t.Error("Peek on empty should return nil")
	}

	q.Push(&TaskEntry{
		ID:   "task1",
		Priority: TaskPriority{Base: 50, EnqueuedAt: time.Now()},
	})

	peeked := q.Peek()
	if peeked == nil || peeked.ID != "task1" {
		t.Error("Peek should return task1")
	}
	if q.Len() != 1 {
		t.Error("Peek should not remove the item")
	}
}

func TestReadyQueue_Aging(t *testing.T) {
	q := NewReadyQueue()

	// Low priority but old.
	q.Push(&TaskEntry{
		ID:   "old-low",
		Priority: TaskPriority{Base: 10, EnqueuedAt: time.Now().Add(-100 * time.Second), AgingFactor: 1.0},
	})
	// High priority but new.
	q.Push(&TaskEntry{
		ID:   "new-high",
		Priority: TaskPriority{Base: 50, EnqueuedAt: time.Now(), AgingFactor: 0.1},
	})

	// old-low: 10 + 100*1.0 = 110
	// new-high: 50 + 0*0.1 = 50
	first := q.Pop()
	if first.ID != "old-low" {
		t.Errorf("aged task should come first, got %q", first.ID)
	}
}

func TestReadyQueue_Drain(t *testing.T) {
	q := NewReadyQueue()
	q.Push(&TaskEntry{ID: "a", Priority: TaskPriority{Base: 10, EnqueuedAt: time.Now()}})
	q.Push(&TaskEntry{ID: "b", Priority: TaskPriority{Base: 20, EnqueuedAt: time.Now()}})
	q.Push(&TaskEntry{ID: "c", Priority: TaskPriority{Base: 30, EnqueuedAt: time.Now()}})

	drained := q.Drain()
	if len(drained) != 3 {
		t.Errorf("Drain len=%d, want 3", len(drained))
	}
	if q.Len() != 0 {
		t.Errorf("queue should be empty after drain, len=%d", q.Len())
	}

	// Should be in priority order.
	if drained[0].ID != "c" {
		t.Errorf("first drained=%q, want c (highest)", drained[0].ID)
	}
}

func TestTaskState_String(t *testing.T) {
	states := []TaskState{TaskPending, TaskAssigned, TaskRunning, TaskCompleted, TaskFailed, TaskCancelled}
	expected := []string{"pending", "assigned", "running", "completed", "failed", "cancelled"}

	for i, s := range states {
		if s.String() != expected[i] {
			t.Errorf("state %d: got %q, want %q", i, s.String(), expected[i])
		}
	}
}
