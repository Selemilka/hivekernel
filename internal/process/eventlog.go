package process

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// EventType identifies what happened to a process.
type EventType string

const (
	EventSpawned      EventType = "spawned"
	EventStateChanged EventType = "state_changed"
	EventRemoved      EventType = "removed"
	EventLogged       EventType = "log"
	EventMessageSent  EventType = "message_sent"
)

// ProcessEvent represents a single mutation in the process registry.
type ProcessEvent struct {
	Seq       uint64    `json:"seq"`
	Timestamp time.Time `json:"ts"`
	Type      EventType `json:"type"`
	PID       PID       `json:"pid"`
	PPID      PID       `json:"ppid,omitempty"`
	Name      string    `json:"name,omitempty"`
	Role      string    `json:"role,omitempty"`
	Tier      string    `json:"tier,omitempty"`
	Model     string    `json:"model,omitempty"`
	State     string    `json:"state,omitempty"`
	OldState  string    `json:"old_state,omitempty"`
	NewState  string    `json:"new_state,omitempty"`
	Level          string    `json:"level,omitempty"`
	Message        string    `json:"message,omitempty"`
	ReplyTo        string    `json:"reply_to,omitempty"`
	PayloadPreview string    `json:"payload_preview,omitempty"`
}

// EventLog is a sequenced, append-only log of process events.
// It supports in-memory ring buffer, optional disk persistence (JSONL),
// and fan-out to live subscribers.
type EventLog struct {
	mu      sync.RWMutex
	events  []ProcessEvent
	seq     atomic.Uint64
	maxSize int

	subs []chan ProcessEvent

	logFile *os.File
	writer  *bufio.Writer
}

// NewEventLog creates an EventLog with the given ring buffer capacity.
// If logPath is non-empty, events are also appended to that file as JSONL.
func NewEventLog(maxSize int, logPath string) (*EventLog, error) {
	el := &EventLog{
		events:  make([]ProcessEvent, 0, maxSize),
		maxSize: maxSize,
	}

	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("open event log: %w", err)
		}
		el.logFile = f
		el.writer = bufio.NewWriter(f)
	}

	return el, nil
}

// Emit assigns a sequence number and timestamp to the event, appends it
// to the ring buffer, writes to disk (if configured), and fans out to
// all live subscribers.
func (el *EventLog) Emit(evt ProcessEvent) {
	evt.Seq = el.seq.Add(1)
	evt.Timestamp = time.Now()

	el.mu.Lock()

	// Append to ring buffer.
	el.events = append(el.events, evt)
	if len(el.events) > el.maxSize {
		// Trim oldest half.
		half := len(el.events) / 2
		el.events = append([]ProcessEvent(nil), el.events[half:]...)
	}

	// Write to disk.
	if el.writer != nil {
		data, err := json.Marshal(evt)
		if err == nil {
			el.writer.Write(data)
			el.writer.WriteByte('\n')
			el.writer.Flush()
		}
	}

	// Snapshot subs under lock.
	subs := make([]chan ProcessEvent, len(el.subs))
	copy(subs, el.subs)
	el.mu.Unlock()

	// Fan-out outside the lock to avoid blocking.
	for _, ch := range subs {
		select {
		case ch <- evt:
		default:
			// Slow consumer — drop event rather than block.
		}
	}
}

// Since returns all buffered events with Seq > sinceSeq.
func (el *EventLog) Since(sinceSeq uint64) []ProcessEvent {
	el.mu.RLock()
	defer el.mu.RUnlock()

	var result []ProcessEvent
	for _, e := range el.events {
		if e.Seq > sinceSeq {
			result = append(result, e)
		}
	}
	return result
}

// CurrentSeq returns the latest sequence number.
func (el *EventLog) CurrentSeq() uint64 {
	return el.seq.Load()
}

// SubscribeSince replays buffered events with Seq > sinceSeq into the
// returned channel, then registers it for live events. This is done
// atomically under the write lock so no events are missed between
// replay and subscription.
func (el *EventLog) SubscribeSince(sinceSeq uint64, bufSize int) chan ProcessEvent {
	ch := make(chan ProcessEvent, bufSize)

	el.mu.Lock()
	defer el.mu.Unlock()

	// Replay buffered events.
	for _, e := range el.events {
		if e.Seq > sinceSeq {
			select {
			case ch <- e:
			default:
			}
		}
	}

	// Register for live events.
	el.subs = append(el.subs, ch)
	return ch
}

// Unsubscribe removes a channel from the subscriber list and closes it.
func (el *EventLog) Unsubscribe(ch chan ProcessEvent) {
	el.mu.Lock()
	defer el.mu.Unlock()

	for i, s := range el.subs {
		if s == ch {
			el.subs = append(el.subs[:i], el.subs[i+1:]...)
			close(ch)
			return
		}
	}
}

// Close flushes and closes the disk log file (if any).
func (el *EventLog) Close() {
	el.mu.Lock()
	defer el.mu.Unlock()

	if el.writer != nil {
		el.writer.Flush()
	}
	if el.logFile != nil {
		el.logFile.Close()
		el.logFile = nil
		el.writer = nil
	}

	// Close all subscriber channels.
	for _, ch := range el.subs {
		close(ch)
	}
	el.subs = nil
}
