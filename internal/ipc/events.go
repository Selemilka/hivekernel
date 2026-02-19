package ipc

import (
	"sync"
	"time"

	"github.com/selemilka/hivekernel/internal/hklog"
	"github.com/selemilka/hivekernel/internal/process"
)

// Event is a broadcast notification sent to all subscribers of a topic.
type Event struct {
	Topic     string
	Source    process.PID
	Payload   []byte
	Timestamp time.Time
}

// EventBus provides pub/sub broadcast events.
// Topics are strings like "vps_down", "budget_exceeded", "process_died".
type EventBus struct {
	mu          sync.RWMutex
	subscribers map[string][]chan Event // topic -> list of subscriber channels
}

// NewEventBus creates a new event bus.
func NewEventBus() *EventBus {
	return &EventBus{
		subscribers: make(map[string][]chan Event),
	}
}

// Subscribe registers a channel to receive events for a topic.
// Returns the channel the caller should read from.
func (eb *EventBus) Subscribe(topic string, bufSize int) chan Event {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	if bufSize <= 0 {
		bufSize = 32
	}
	ch := make(chan Event, bufSize)
	eb.subscribers[topic] = append(eb.subscribers[topic], ch)
	return ch
}

// Unsubscribe removes a channel from a topic.
func (eb *EventBus) Unsubscribe(topic string, ch chan Event) {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	subs := eb.subscribers[topic]
	for i, s := range subs {
		if s == ch {
			eb.subscribers[topic] = append(subs[:i], subs[i+1:]...)
			close(ch)
			return
		}
	}
}

// Publish sends an event to all subscribers of the topic.
// Non-blocking: if a subscriber's channel is full, the event is dropped for that subscriber.
func (eb *EventBus) Publish(topic string, source process.PID, payload []byte) {
	eb.mu.RLock()
	subs := eb.subscribers[topic]
	eb.mu.RUnlock()

	evt := Event{
		Topic:     topic,
		Source:    source,
		Payload:   payload,
		Timestamp: time.Now(),
	}

	delivered := 0
	for _, ch := range subs {
		select {
		case ch <- evt:
			delivered++
		default:
			// Channel full, drop for this subscriber.
		}
	}

	if len(subs) > 0 {
		hklog.For("events").Debug("published event", "topic", topic, "source_pid", source, "delivered", delivered, "total_subs", len(subs))
	}
}

// TopicCount returns the number of subscribers for a topic.
func (eb *EventBus) TopicCount(topic string) int {
	eb.mu.RLock()
	defer eb.mu.RUnlock()
	return len(eb.subscribers[topic])
}
