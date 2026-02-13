package scheduler

import (
	"testing"

	"github.com/selemilka/hivekernel/internal/process"
)

func TestScheduler_Submit(t *testing.T) {
	sched := NewScheduler(0.1)

	entry, err := sched.Submit("compile-frontend", process.CogTactical, process.RoleWorker, 100, []byte("build frontend"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if entry.ID == "" {
		t.Error("task ID should not be empty")
	}
	if entry.State != TaskPending {
		t.Errorf("State=%s, want pending", entry.State)
	}
	if sched.PendingCount() != 1 {
		t.Errorf("PendingCount=%d, want 1", sched.PendingCount())
	}
}

func TestScheduler_Submit_EmptyName(t *testing.T) {
	sched := NewScheduler(0.1)

	_, err := sched.Submit("", process.CogTactical, process.RoleWorker, 100, nil)
	if err == nil {
		t.Error("empty name should fail")
	}
}

func TestScheduler_Assign(t *testing.T) {
	sched := NewScheduler(0.1)

	sched.Submit("task-a", process.CogTactical, process.RoleWorker, 100, nil)
	sched.Submit("task-b", process.CogStrategic, process.RoleLead, 100, nil)

	// Strategic agent should get strategic task first (higher priority).
	assigned := sched.Assign(200, process.CogStrategic, process.RoleKernel)
	if assigned == nil {
		t.Fatal("should assign a task")
	}
	if assigned.Name != "task-b" {
		t.Errorf("assigned=%q, want task-b (higher priority)", assigned.Name)
	}
	if assigned.AssignedTo != 200 {
		t.Errorf("AssignedTo=%d, want 200", assigned.AssignedTo)
	}
	if assigned.State != TaskAssigned {
		t.Errorf("State=%s, want assigned", assigned.State)
	}

	if sched.PendingCount() != 1 {
		t.Errorf("PendingCount=%d, want 1", sched.PendingCount())
	}
}

func TestScheduler_Assign_TierMismatch(t *testing.T) {
	sched := NewScheduler(0.1)

	// Submit a strategic task.
	sched.Submit("strategic-task", process.CogStrategic, process.RoleLead, 100, nil)

	// Operational agent cannot handle strategic task.
	assigned := sched.Assign(300, process.CogOperational, process.RoleTask)
	if assigned != nil {
		t.Error("operational agent should not be assigned strategic task")
	}

	// Task should still be in queue.
	if sched.PendingCount() != 1 {
		t.Errorf("PendingCount=%d, want 1 (task should remain)", sched.PendingCount())
	}
}

func TestScheduler_Assign_RoleMismatch(t *testing.T) {
	sched := NewScheduler(0.1)

	// Submit a lead-level task.
	sched.Submit("lead-task", process.CogTactical, process.RoleLead, 100, nil)

	// Task role cannot handle lead work.
	assigned := sched.Assign(300, process.CogTactical, process.RoleTask)
	if assigned != nil {
		t.Error("task-role agent should not be assigned lead-level task")
	}
}

func TestScheduler_Assign_NoTasks(t *testing.T) {
	sched := NewScheduler(0.1)

	assigned := sched.Assign(200, process.CogStrategic, process.RoleKernel)
	if assigned != nil {
		t.Error("should return nil when no tasks available")
	}
}

func TestScheduler_Complete(t *testing.T) {
	sched := NewScheduler(0.1)

	entry, _ := sched.Submit("task-a", process.CogTactical, process.RoleWorker, 100, nil)
	sched.Assign(200, process.CogTactical, process.RoleWorker)

	err := sched.Complete(entry.ID, true)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	task, _ := sched.GetTask(entry.ID)
	if task.State != TaskCompleted {
		t.Errorf("State=%s, want completed", task.State)
	}
}

func TestScheduler_Complete_Failure(t *testing.T) {
	sched := NewScheduler(0.1)

	entry, _ := sched.Submit("task-a", process.CogTactical, process.RoleWorker, 100, nil)
	sched.Assign(200, process.CogTactical, process.RoleWorker)

	sched.Complete(entry.ID, false)

	task, _ := sched.GetTask(entry.ID)
	if task.State != TaskFailed {
		t.Errorf("State=%s, want failed", task.State)
	}
}

func TestScheduler_Cancel(t *testing.T) {
	sched := NewScheduler(0.1)

	entry, _ := sched.Submit("task-a", process.CogTactical, process.RoleWorker, 100, nil)

	err := sched.Cancel(entry.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	task, _ := sched.GetTask(entry.ID)
	if task.State != TaskCancelled {
		t.Errorf("State=%s, want cancelled", task.State)
	}
}

func TestScheduler_Cancel_Completed(t *testing.T) {
	sched := NewScheduler(0.1)

	entry, _ := sched.Submit("task-a", process.CogTactical, process.RoleWorker, 100, nil)
	sched.Assign(200, process.CogTactical, process.RoleWorker)
	sched.Complete(entry.ID, true)

	err := sched.Cancel(entry.ID)
	if err == nil {
		t.Error("should not cancel completed task")
	}
}

func TestScheduler_Resubmit(t *testing.T) {
	sched := NewScheduler(0.1)

	entry, _ := sched.Submit("task-a", process.CogTactical, process.RoleWorker, 100, nil)
	sched.Assign(200, process.CogTactical, process.RoleWorker)
	sched.Complete(entry.ID, false) // fail

	err := sched.Resubmit(entry.ID)
	if err != nil {
		t.Fatalf("Resubmit: %v", err)
	}

	task, _ := sched.GetTask(entry.ID)
	if task.State != TaskPending {
		t.Errorf("State=%s, want pending", task.State)
	}
	if task.AssignedTo != 0 {
		t.Errorf("AssignedTo=%d, want 0 (unassigned)", task.AssignedTo)
	}
	if sched.PendingCount() != 1 {
		t.Errorf("PendingCount=%d, want 1", sched.PendingCount())
	}
}

func TestScheduler_Resubmit_NotFailed(t *testing.T) {
	sched := NewScheduler(0.1)

	entry, _ := sched.Submit("task-a", process.CogTactical, process.RoleWorker, 100, nil)

	err := sched.Resubmit(entry.ID)
	if err == nil {
		t.Error("should not resubmit pending task")
	}
}

func TestScheduler_TasksByState(t *testing.T) {
	sched := NewScheduler(0.1)

	sched.Submit("task-a", process.CogTactical, process.RoleWorker, 100, nil)
	sched.Submit("task-b", process.CogTactical, process.RoleWorker, 100, nil)
	entry, _ := sched.Submit("task-c", process.CogTactical, process.RoleWorker, 100, nil)

	sched.Assign(200, process.CogTactical, process.RoleWorker)
	sched.Complete(entry.ID, true)

	pending := sched.TasksByState(TaskPending)
	// task-a was assigned, task-b still pending, task-c completed.
	if len(pending) != 1 {
		t.Errorf("pending=%d, want 1", len(pending))
	}

	completed := sched.TasksByState(TaskCompleted)
	if len(completed) != 1 {
		t.Errorf("completed=%d, want 1", len(completed))
	}
}

func TestScheduler_TasksByAgent(t *testing.T) {
	sched := NewScheduler(0.1)

	sched.Submit("task-a", process.CogTactical, process.RoleWorker, 100, nil)
	sched.Submit("task-b", process.CogTactical, process.RoleWorker, 100, nil)

	sched.Assign(200, process.CogTactical, process.RoleWorker)
	sched.Assign(200, process.CogTactical, process.RoleWorker)

	tasks := sched.TasksByAgent(200)
	if len(tasks) != 2 {
		t.Errorf("TasksByAgent(200)=%d, want 2", len(tasks))
	}

	tasks = sched.TasksByAgent(999)
	if len(tasks) != 0 {
		t.Errorf("TasksByAgent(999)=%d, want 0", len(tasks))
	}
}

func TestScheduler_PriorityOrdering(t *testing.T) {
	sched := NewScheduler(0.1)

	// Submit tasks with different tiers.
	sched.Submit("operational", process.CogOperational, process.RoleTask, 100, nil)
	sched.Submit("strategic", process.CogStrategic, process.RoleLead, 100, nil)
	sched.Submit("tactical", process.CogTactical, process.RoleWorker, 100, nil)

	// Strategic agent should get strategic first.
	first := sched.Assign(200, process.CogStrategic, process.RoleKernel)
	if first.Name != "strategic" {
		t.Errorf("first=%q, want strategic", first.Name)
	}

	second := sched.Assign(201, process.CogStrategic, process.RoleKernel)
	if second.Name != "tactical" {
		t.Errorf("second=%q, want tactical", second.Name)
	}

	third := sched.Assign(202, process.CogStrategic, process.RoleKernel)
	if third.Name != "operational" {
		t.Errorf("third=%q, want operational", third.Name)
	}
}
