package scheduler

import (
	"fmt"
	"sync"
	"time"

	"github.com/selemilka/hivekernel/internal/process"
)

// Scheduler manages task dispatch across the process tree.
// It matches submitted tasks to available agents based on cognitive tier,
// role requirements, and priority ordering.
type Scheduler struct {
	mu          sync.RWMutex
	queue       *ReadyQueue
	tasks       map[string]*TaskEntry // id -> task
	agingFactor float64
	nextID      int
}

// NewScheduler creates a scheduler with the given aging factor.
func NewScheduler(agingFactor float64) *Scheduler {
	return &Scheduler{
		queue:       NewReadyQueue(),
		tasks:       make(map[string]*TaskEntry),
		agingFactor: agingFactor,
	}
}

// Submit adds a new task to the ready queue.
func (s *Scheduler) Submit(name string, requiredTier process.CognitiveTier, requiredRole process.AgentRole, submittedBy process.PID, payload []byte) (*TaskEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if name == "" {
		return nil, fmt.Errorf("task name is required")
	}

	s.nextID++
	id := fmt.Sprintf("task-%d", s.nextID)

	entry := &TaskEntry{
		ID:           id,
		Name:         name,
		RequiredTier: requiredTier,
		RequiredRole: requiredRole,
		Priority: TaskPriority{
			Base:        ComputeBasePriority(requiredTier, requiredRole),
			EnqueuedAt:  time.Now(),
			AgingFactor: s.agingFactor,
		},
		Payload:     payload,
		SubmittedBy: submittedBy,
		State:       TaskPending,
	}

	s.tasks[id] = entry
	s.queue.Push(entry)
	return entry, nil
}

// Assign assigns the highest-priority pending task to the given agent.
// Returns nil if no suitable task is available.
func (s *Scheduler) Assign(agentPID process.PID, agentTier process.CognitiveTier, agentRole process.AgentRole) *TaskEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Drain and re-insert to find a matching task.
	var requeue []*TaskEntry
	var found *TaskEntry

	for {
		entry := s.queue.Pop()
		if entry == nil {
			break
		}

		if found == nil && s.isCompatible(entry, agentTier, agentRole) {
			entry.AssignedTo = agentPID
			entry.State = TaskAssigned
			found = entry
		} else {
			requeue = append(requeue, entry)
		}
	}

	// Put back unmatched tasks.
	for _, e := range requeue {
		s.queue.Push(e)
	}

	return found
}

// Next returns the highest-priority pending task without assigning it.
func (s *Scheduler) Next() *TaskEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.queue.Peek()
}

// isCompatible checks if an agent can handle a task.
func (s *Scheduler) isCompatible(task *TaskEntry, agentTier process.CognitiveTier, agentRole process.AgentRole) bool {
	// Agent's cognitive tier must be >= task requirement.
	// (Lower number = higher tier: CogStrategic=0 is highest)
	if agentTier > task.RequiredTier {
		return false
	}

	// Role compatibility: any role can handle a task role request.
	// But leads should handle lead tasks, workers handle worker tasks.
	// For simplicity, exact role match or agent role is higher.
	if agentRole > task.RequiredRole {
		return false
	}

	return true
}

// Complete marks a task as completed.
func (s *Scheduler) Complete(taskID string, success bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %q not found", taskID)
	}

	if success {
		task.State = TaskCompleted
	} else {
		task.State = TaskFailed
	}
	return nil
}

// Cancel cancels a pending or assigned task.
func (s *Scheduler) Cancel(taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %q not found", taskID)
	}

	if task.State == TaskCompleted || task.State == TaskFailed {
		return fmt.Errorf("cannot cancel %s task", task.State)
	}

	task.State = TaskCancelled
	return nil
}

// Resubmit puts a failed or cancelled task back into the queue.
func (s *Scheduler) Resubmit(taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %q not found", taskID)
	}

	if task.State != TaskFailed && task.State != TaskCancelled {
		return fmt.Errorf("can only resubmit failed or cancelled tasks, got %s", task.State)
	}

	task.State = TaskPending
	task.AssignedTo = 0
	task.Priority.EnqueuedAt = time.Now()
	s.queue.Push(task)
	return nil
}

// GetTask returns a task by ID.
func (s *Scheduler) GetTask(taskID string) (*TaskEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return nil, fmt.Errorf("task %q not found", taskID)
	}
	return task, nil
}

// PendingCount returns the number of tasks in the ready queue.
func (s *Scheduler) PendingCount() int {
	return s.queue.Len()
}

// TasksByState returns all tasks matching the given state.
func (s *Scheduler) TasksByState(state TaskState) []*TaskEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*TaskEntry
	for _, t := range s.tasks {
		if t.State == state {
			result = append(result, t)
		}
	}
	return result
}

// TasksByAgent returns all tasks assigned to a given agent PID.
func (s *Scheduler) TasksByAgent(pid process.PID) []*TaskEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*TaskEntry
	for _, t := range s.tasks {
		if t.AssignedTo == pid {
			result = append(result, t)
		}
	}
	return result
}
