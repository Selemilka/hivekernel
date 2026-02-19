package ipc

import (
	"fmt"
	"log"
	"sync"

	"github.com/selemilka/hivekernel/internal/process"
)

// Broker is the central message router. It validates routing rules,
// computes priorities, and delivers messages to per-process inboxes.
type Broker struct {
	mu        sync.RWMutex
	registry  *process.Registry
	inboxes   map[process.PID]*PriorityQueue // per-process inbox
	queues    map[string]*PriorityQueue      // named queues (for Subscribe)
	aging     float64
	OnMessage func(pid process.PID, msg *Message) // push delivery callback
}

// NewBroker creates a message broker backed by the given registry.
func NewBroker(registry *process.Registry, agingFactor float64) *Broker {
	return &Broker{
		registry: registry,
		inboxes:  make(map[process.PID]*PriorityQueue),
		queues:   make(map[string]*PriorityQueue),
		aging:    agingFactor,
	}
}

// GetInbox returns (or creates) the inbox queue for a process.
func (b *Broker) GetInbox(pid process.PID) *PriorityQueue {
	b.mu.Lock()
	defer b.mu.Unlock()

	if q, ok := b.inboxes[pid]; ok {
		return q
	}
	q := NewPriorityQueue(b.aging)
	b.inboxes[pid] = q
	return q
}

// GetNamedQueue returns (or creates) a named queue for pub/sub.
func (b *Broker) GetNamedQueue(name string) *PriorityQueue {
	b.mu.Lock()
	defer b.mu.Unlock()

	if q, ok := b.queues[name]; ok {
		return q
	}
	q := NewPriorityQueue(b.aging)
	b.queues[name] = q
	return q
}

// Route validates routing rules, computes priority, and delivers a message.
func (b *Broker) Route(msg *Message) error {
	// Validate routing rules.
	if err := b.validateRoute(msg); err != nil {
		return err
	}

	// Compute relationship-based priority.
	rel := DetermineRelationship(b.registry, msg.FromPID, msg.ToPID)
	msg.Priority = ComputeEffectivePriority(msg.Priority, rel)

	// Determine delivery path.
	if msg.ToQueue != "" {
		// Named queue delivery.
		q := b.GetNamedQueue(msg.ToQueue)
		q.Push(msg)
		log.Printf("[broker] PID %d -> queue %q (type=%s, priority=%d)",
			msg.FromPID, msg.ToQueue, msg.Type, msg.Priority)
		return nil
	}

	if msg.ToPID != 0 {
		// Direct delivery to process inbox.
		route, err := b.findRoute(msg.FromPID, msg.ToPID)
		if err != nil {
			return err
		}

		// For sibling messages, also copy to parent's inbox (parent sees traffic).
		if route == routeSibling {
			b.deliverToParentCopy(msg)
		}

		q := b.GetInbox(msg.ToPID)
		q.Push(msg)
		log.Printf("[broker] PID %d -> PID %d (route=%s, type=%s, priority=%d)",
			msg.FromPID, msg.ToPID, route, msg.Type, msg.Priority)

		// Push delivery: notify agent runtime immediately.
		if b.OnMessage != nil {
			go b.OnMessage(msg.ToPID, msg)
		}
		return nil
	}

	return fmt.Errorf("message has no target (to_pid=0, to_queue empty)")
}

// routeType describes how a message is routed.
type routeType string

const (
	routeDirect   routeType = "direct"   // parent <-> child
	routeSibling  routeType = "sibling"  // siblings, parent sees
	routeAncestor routeType = "ancestor" // through nearest common ancestor
)

// validateRoute enforces IPC routing rules from the spec.
func (b *Broker) validateRoute(msg *Message) error {
	if msg.ToPID == 0 && msg.ToQueue == "" {
		return fmt.Errorf("no target specified")
	}

	// Named queue: always allowed (broker manages access).
	if msg.ToQueue != "" {
		return nil
	}

	sender, err := b.registry.Get(msg.FromPID)
	if err != nil {
		return fmt.Errorf("sender PID %d not found", msg.FromPID)
	}

	_, err = b.registry.Get(msg.ToPID)
	if err != nil {
		return fmt.Errorf("receiver PID %d not found", msg.ToPID)
	}

	// Rule: Task can only send to parent (escalation only).
	if sender.Role == process.RoleTask {
		if sender.PPID != msg.ToPID {
			return fmt.Errorf("task (PID %d) can only send to its parent (PID %d), not PID %d",
				msg.FromPID, sender.PPID, msg.ToPID)
		}
	}

	return nil
}

// findRoute determines the routing path between two processes.
func (b *Broker) findRoute(fromPID, toPID process.PID) (routeType, error) {
	sender, err := b.registry.Get(fromPID)
	if err != nil {
		return "", err
	}
	receiver, err := b.registry.Get(toPID)
	if err != nil {
		return "", err
	}

	// Parent -> child or child -> parent: direct.
	if receiver.PPID == fromPID || sender.PPID == toPID {
		return routeDirect, nil
	}

	// Siblings: same parent.
	if sender.PPID == receiver.PPID && sender.PPID != 0 {
		return routeSibling, nil
	}

	// Different branches: route through nearest common ancestor.
	_, found := b.registry.NearestCommonAncestor(fromPID, toPID)
	if found {
		return routeAncestor, nil
	}

	return "", fmt.Errorf("no route from PID %d to PID %d", fromPID, toPID)
}

// deliverToParentCopy copies sibling messages to the parent's inbox.
func (b *Broker) deliverToParentCopy(msg *Message) {
	sender, err := b.registry.Get(msg.FromPID)
	if err != nil {
		return
	}

	parentCopy := &Message{
		FromPID:     msg.FromPID,
		FromName:    msg.FromName,
		ToPID:       msg.ToPID,
		ToQueue:     msg.ToQueue,
		Type:        "sibling_traffic:" + msg.Type,
		Priority:    PriorityLow, // parent gets a low-priority copy
		Payload:     msg.Payload,
		RequiresAck: false,
		ReplyTo:     msg.ReplyTo,
	}

	parentQ := b.GetInbox(sender.PPID)
	parentQ.Push(parentCopy)
}

// RemoveInbox cleans up when a process dies.
func (b *Broker) RemoveInbox(pid process.PID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.inboxes, pid)
}

// InboxLen returns the number of messages in a process's inbox.
func (b *Broker) InboxLen(pid process.PID) int {
	b.mu.RLock()
	q, ok := b.inboxes[pid]
	b.mu.RUnlock()
	if !ok {
		return 0
	}
	return q.Len()
}

// ListInbox returns a snapshot of messages currently in a PID's inbox
// without removing them (non-destructive peek).
func (b *Broker) ListInbox(pid process.PID) []*Message {
	b.mu.RLock()
	q, ok := b.inboxes[pid]
	b.mu.RUnlock()
	if !ok {
		return nil
	}
	return q.Peek()
}

// FlushInbox pops all messages from a PID's inbox and re-delivers each
// via the OnMessage callback. Used after agent init to deliver messages
// that arrived before the agent was ready.
func (b *Broker) FlushInbox(pid process.PID) int {
	b.mu.RLock()
	q, ok := b.inboxes[pid]
	b.mu.RUnlock()
	if !ok || b.OnMessage == nil {
		return 0
	}

	count := 0
	for {
		msg := q.Pop()
		if msg == nil {
			break
		}
		go b.OnMessage(pid, msg)
		count++
	}
	return count
}
