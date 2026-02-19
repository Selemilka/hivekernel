package ipc

import (
	"container/heap"
	"sync"
	"sync/atomic"
	"time"

	"github.com/selemilka/hivekernel/internal/process"
)

// Priority levels (lower number = higher priority, matching proto enum).
const (
	PriorityCritical = 0
	PriorityHigh     = 1
	PriorityNormal   = 2
	PriorityLow      = 3
)

// DefaultAgingFactor controls how fast old messages gain priority.
// Units: priority points per second.
const DefaultAgingFactor = 0.1

// Message represents an IPC message in the queue.
type Message struct {
	ID         string
	FromPID    process.PID
	FromName   string
	ToPID      process.PID
	ToQueue    string
	Type       string
	Priority   int
	Payload    []byte
	RequiresAck bool
	ReplyTo    string // correlation ID for request-response patterns
	TTL        time.Duration // 0 = no expiry
	EnqueuedAt time.Time
	ExpiresAt  time.Time // zero value = no expiry
}

// EffectivePriority computes priority with aging.
// Lower value = higher priority.
func (m *Message) EffectivePriority(agingFactor float64) float64 {
	age := time.Since(m.EnqueuedAt).Seconds()
	return float64(m.Priority) - age*agingFactor
}

// --- Priority queue implementation via container/heap ---

type messageHeap struct {
	items       []*Message
	agingFactor float64
}

func (h *messageHeap) Len() int { return len(h.items) }

func (h *messageHeap) Less(i, j int) bool {
	// Lower effective priority = should be popped first.
	return h.items[i].EffectivePriority(h.agingFactor) <
		h.items[j].EffectivePriority(h.agingFactor)
}

func (h *messageHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
}

func (h *messageHeap) Push(x any) {
	h.items = append(h.items, x.(*Message))
}

func (h *messageHeap) Pop() any {
	old := h.items
	n := len(old)
	item := old[n-1]
	old[n-1] = nil // avoid memory leak
	h.items = old[:n-1]
	return item
}

// PriorityQueue is a thread-safe priority queue with aging for IPC messages.
type PriorityQueue struct {
	mu          sync.Mutex
	h           *messageHeap
	notify      chan struct{} // signaled on Push so Pop can block
	nextID      atomic.Uint64
	agingFactor float64
}

// NewPriorityQueue creates a new queue with the given aging factor.
func NewPriorityQueue(agingFactor float64) *PriorityQueue {
	pq := &PriorityQueue{
		h: &messageHeap{
			agingFactor: agingFactor,
		},
		notify:      make(chan struct{}, 1),
		agingFactor: agingFactor,
	}
	heap.Init(pq.h)
	return pq
}

// Push adds a message to the queue.
func (pq *PriorityQueue) Push(msg *Message) {
	pq.mu.Lock()
	if msg.EnqueuedAt.IsZero() {
		msg.EnqueuedAt = time.Now()
	}
	if msg.TTL > 0 && msg.ExpiresAt.IsZero() {
		msg.ExpiresAt = msg.EnqueuedAt.Add(msg.TTL)
	}
	if msg.ID == "" {
		msg.ID = pq.generateID()
	}
	heap.Push(pq.h, msg)
	pq.mu.Unlock()

	// Non-blocking notify.
	select {
	case pq.notify <- struct{}{}:
	default:
	}
}

// Pop removes and returns the highest-priority message.
// Skips expired messages. Returns nil if the queue is empty.
func (pq *PriorityQueue) Pop() *Message {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	for pq.h.Len() > 0 {
		// Re-heapify since effective priorities change over time.
		heap.Init(pq.h)
		msg := heap.Pop(pq.h).(*Message)

		// Skip expired messages.
		if !msg.ExpiresAt.IsZero() && time.Now().After(msg.ExpiresAt) {
			continue
		}
		return msg
	}
	return nil
}

// PopWait blocks until a message is available or the done channel is closed.
func (pq *PriorityQueue) PopWait(done <-chan struct{}) *Message {
	for {
		if msg := pq.Pop(); msg != nil {
			return msg
		}
		select {
		case <-pq.notify:
		case <-done:
			return nil
		}
	}
}

// Len returns the current queue length.
func (pq *PriorityQueue) Len() int {
	pq.mu.Lock()
	defer pq.mu.Unlock()
	return pq.h.Len()
}

// Peek returns a copy of all messages in the queue without removing them.
func (pq *PriorityQueue) Peek() []*Message {
	pq.mu.Lock()
	defer pq.mu.Unlock()
	result := make([]*Message, len(pq.h.items))
	copy(result, pq.h.items)
	return result
}

func (pq *PriorityQueue) generateID() string {
	n := pq.nextID.Add(1)
	return "msg-" + uitoa(n)
}

func uitoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
