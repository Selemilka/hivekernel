package ipc

import (
	"testing"
	"time"
)

func TestPriorityQueueBasic(t *testing.T) {
	pq := NewPriorityQueue(0) // no aging

	pq.Push(&Message{Priority: PriorityLow, Type: "low"})
	pq.Push(&Message{Priority: PriorityCritical, Type: "critical"})
	pq.Push(&Message{Priority: PriorityNormal, Type: "normal"})

	// Should come out in priority order: critical, normal, low.
	msg := pq.Pop()
	if msg.Type != "critical" {
		t.Fatalf("expected critical first, got %s", msg.Type)
	}
	msg = pq.Pop()
	if msg.Type != "normal" {
		t.Fatalf("expected normal second, got %s", msg.Type)
	}
	msg = pq.Pop()
	if msg.Type != "low" {
		t.Fatalf("expected low third, got %s", msg.Type)
	}

	// Empty queue returns nil.
	if pq.Pop() != nil {
		t.Fatal("expected nil from empty queue")
	}
}

func TestPriorityQueueAging(t *testing.T) {
	pq := NewPriorityQueue(10.0) // aggressive aging: 10 points/sec

	// Push a low-priority message with old timestamp.
	old := &Message{
		Priority:   PriorityLow, // base = 3
		Type:       "old-low",
		EnqueuedAt: time.Now().Add(-1 * time.Second), // 1 sec ago
	}
	pq.Push(old)

	// Push a normal-priority message just now.
	fresh := &Message{
		Priority: PriorityNormal, // base = 2
		Type:     "fresh-normal",
	}
	pq.Push(fresh)

	// Old message: effective = 3 - 10*1 = -7 (very high priority)
	// Fresh message: effective = 2 - 10*~0 = ~2
	// Old message should win.
	msg := pq.Pop()
	if msg.Type != "old-low" {
		t.Fatalf("expected aged old-low message first, got %s (aging should promote it)", msg.Type)
	}
}

func TestPriorityQueueTTL(t *testing.T) {
	pq := NewPriorityQueue(0)

	// Push a message that's already expired.
	pq.Push(&Message{
		Priority:   PriorityCritical,
		Type:       "expired",
		EnqueuedAt: time.Now().Add(-10 * time.Second),
		TTL:        1 * time.Second,
		ExpiresAt:  time.Now().Add(-9 * time.Second),
	})

	// Push a valid message.
	pq.Push(&Message{Priority: PriorityNormal, Type: "valid"})

	// Expired message should be skipped.
	msg := pq.Pop()
	if msg == nil {
		t.Fatal("expected a message")
	}
	if msg.Type != "valid" {
		t.Fatalf("expected valid, got %s", msg.Type)
	}
}

func TestPriorityQueuePopWait(t *testing.T) {
	pq := NewPriorityQueue(0)

	done := make(chan struct{})
	result := make(chan *Message, 1)

	go func() {
		result <- pq.PopWait(done)
	}()

	// Push after a short delay.
	time.Sleep(50 * time.Millisecond)
	pq.Push(&Message{Priority: PriorityNormal, Type: "delayed"})

	select {
	case msg := <-result:
		if msg == nil || msg.Type != "delayed" {
			t.Fatalf("expected delayed message, got %v", msg)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("PopWait timed out")
	}
}

func TestPriorityQueuePopWaitCancelled(t *testing.T) {
	pq := NewPriorityQueue(0)
	done := make(chan struct{})

	result := make(chan *Message, 1)
	go func() {
		result <- pq.PopWait(done)
	}()

	// Cancel.
	close(done)

	select {
	case msg := <-result:
		if msg != nil {
			t.Fatal("expected nil on cancel")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("PopWait didn't return on cancel")
	}
}

func TestPriorityQueue_Peek(t *testing.T) {
	pq := NewPriorityQueue(0)

	pq.Push(&Message{Priority: PriorityLow, Type: "low"})
	pq.Push(&Message{Priority: PriorityCritical, Type: "critical"})
	pq.Push(&Message{Priority: PriorityNormal, Type: "normal"})

	// Peek should return all 3 messages without removing them.
	peeked := pq.Peek()
	if len(peeked) != 3 {
		t.Fatalf("expected 3 messages from Peek, got %d", len(peeked))
	}

	// Queue should still have all 3.
	if pq.Len() != 3 {
		t.Fatalf("expected queue to still have 3 messages after Peek, got %d", pq.Len())
	}

	// Pop should still work after Peek.
	msg := pq.Pop()
	if msg == nil {
		t.Fatal("expected a message after Peek")
	}
	if pq.Len() != 2 {
		t.Fatalf("expected 2 after one Pop, got %d", pq.Len())
	}
}

func TestPriorityQueue_PeekEmpty(t *testing.T) {
	pq := NewPriorityQueue(0)

	peeked := pq.Peek()
	if len(peeked) != 0 {
		t.Fatalf("expected 0 messages from empty queue Peek, got %d", len(peeked))
	}
}

func TestPriorityQueueIDGeneration(t *testing.T) {
	pq := NewPriorityQueue(0)

	pq.Push(&Message{Type: "a"})
	pq.Push(&Message{Type: "b"})

	a := pq.Pop()
	b := pq.Pop()

	if a.ID == b.ID {
		t.Fatalf("messages should have different IDs, both got %s", a.ID)
	}
	if a.ID == "" || b.ID == "" {
		t.Fatal("message IDs should not be empty")
	}
}
