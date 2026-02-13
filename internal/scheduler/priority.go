package scheduler

import (
	"container/heap"
	"sync"
	"time"

	"github.com/selemilka/hivekernel/internal/process"
)

// TaskPriority computes effective priority for task scheduling.
// Higher effective priority = dispatched sooner.
type TaskPriority struct {
	Base      int       // base priority (from cognitive tier + role)
	EnqueuedAt time.Time
	AgingFactor float64 // how fast priority grows with age
}

// Effective returns the current effective priority with aging.
func (p TaskPriority) Effective() float64 {
	age := time.Since(p.EnqueuedAt).Seconds()
	return float64(p.Base) + age*p.AgingFactor
}

// BasePriorityFromTier returns a base priority based on cognitive tier.
// Strategic tasks get highest priority (they block more work).
func BasePriorityFromTier(tier process.CognitiveTier) int {
	switch tier {
	case process.CogStrategic:
		return 100
	case process.CogTactical:
		return 50
	case process.CogOperational:
		return 10
	default:
		return 10
	}
}

// BasePriorityFromRole returns priority bonus based on role.
func BasePriorityFromRole(role process.AgentRole) int {
	switch role {
	case process.RoleKernel:
		return 50
	case process.RoleDaemon:
		return 40
	case process.RoleAgent:
		return 30
	case process.RoleArchitect:
		return 25
	case process.RoleLead:
		return 20
	case process.RoleWorker:
		return 10
	case process.RoleTask:
		return 5
	default:
		return 0
	}
}

// ComputeBasePriority combines tier and role into a single base priority.
func ComputeBasePriority(tier process.CognitiveTier, role process.AgentRole) int {
	return BasePriorityFromTier(tier) + BasePriorityFromRole(role)
}

// --- Priority Queue for Tasks ---

// TaskEntry represents a task waiting to be dispatched.
type TaskEntry struct {
	ID            string
	Name          string
	RequiredTier  process.CognitiveTier
	RequiredRole  process.AgentRole
	Priority      TaskPriority
	Payload       []byte
	SubmittedBy   process.PID
	AssignedTo    process.PID // 0 = unassigned
	State         TaskState

	// Heap internals.
	index int
}

// TaskState tracks the lifecycle of a scheduled task.
type TaskState int

const (
	TaskPending    TaskState = iota // waiting for assignment
	TaskAssigned                    // assigned to an agent
	TaskRunning                     // agent is executing
	TaskCompleted                   // finished successfully
	TaskFailed                      // finished with error
	TaskCancelled                   // cancelled before completion
)

func (s TaskState) String() string {
	switch s {
	case TaskPending:
		return "pending"
	case TaskAssigned:
		return "assigned"
	case TaskRunning:
		return "running"
	case TaskCompleted:
		return "completed"
	case TaskFailed:
		return "failed"
	case TaskCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

// taskHeap implements heap.Interface for priority-ordered task dispatch.
type taskHeap []*TaskEntry

func (h taskHeap) Len() int { return len(h) }

func (h taskHeap) Less(i, j int) bool {
	// Higher effective priority = dispatched first.
	return h[i].Priority.Effective() > h[j].Priority.Effective()
}

func (h taskHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *taskHeap) Push(x interface{}) {
	n := len(*h)
	entry := x.(*TaskEntry)
	entry.index = n
	*h = append(*h, entry)
}

func (h *taskHeap) Pop() interface{} {
	old := *h
	n := len(old)
	entry := old[n-1]
	old[n-1] = nil
	entry.index = -1
	*h = old[:n-1]
	return entry
}

// ReadyQueue is a thread-safe priority queue for task dispatch.
type ReadyQueue struct {
	mu   sync.Mutex
	heap taskHeap
}

// NewReadyQueue creates an empty ready queue.
func NewReadyQueue() *ReadyQueue {
	rq := &ReadyQueue{}
	heap.Init(&rq.heap)
	return rq
}

// Push adds a task to the ready queue.
func (q *ReadyQueue) Push(entry *TaskEntry) {
	q.mu.Lock()
	defer q.mu.Unlock()
	heap.Push(&q.heap, entry)
}

// Pop removes and returns the highest-priority task.
// Returns nil if the queue is empty.
func (q *ReadyQueue) Pop() *TaskEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.heap.Len() == 0 {
		return nil
	}
	return heap.Pop(&q.heap).(*TaskEntry)
}

// Peek returns the highest-priority task without removing it.
func (q *ReadyQueue) Peek() *TaskEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.heap.Len() == 0 {
		return nil
	}
	return q.heap[0]
}

// Len returns the number of tasks in the queue.
func (q *ReadyQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.heap.Len()
}

// Drain removes and returns all tasks from the queue (highest priority first).
func (q *ReadyQueue) Drain() []*TaskEntry {
	q.mu.Lock()
	defer q.mu.Unlock()

	result := make([]*TaskEntry, 0, q.heap.Len())
	for q.heap.Len() > 0 {
		result = append(result, heap.Pop(&q.heap).(*TaskEntry))
	}
	return result
}
