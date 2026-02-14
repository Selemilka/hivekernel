package process

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEmitAndSince(t *testing.T) {
	el, err := NewEventLog(100, "")
	if err != nil {
		t.Fatal(err)
	}
	defer el.Close()

	for i := 0; i < 5; i++ {
		el.Emit(ProcessEvent{Type: EventSpawned, PID: uint64(i + 1)})
	}

	if el.CurrentSeq() != 5 {
		t.Fatalf("expected seq 5, got %d", el.CurrentSeq())
	}

	// Since(3) should return events with seq 4 and 5.
	evts := el.Since(3)
	if len(evts) != 2 {
		t.Fatalf("expected 2 events since seq 3, got %d", len(evts))
	}
	if evts[0].Seq != 4 || evts[1].Seq != 5 {
		t.Fatalf("expected seqs 4,5; got %d,%d", evts[0].Seq, evts[1].Seq)
	}

	// Since(0) returns all.
	evts = el.Since(0)
	if len(evts) != 5 {
		t.Fatalf("expected 5 events since seq 0, got %d", len(evts))
	}

	// Since(5) returns none.
	evts = el.Since(5)
	if len(evts) != 0 {
		t.Fatalf("expected 0 events since seq 5, got %d", len(evts))
	}
}

func TestSubscribeSince(t *testing.T) {
	el, err := NewEventLog(100, "")
	if err != nil {
		t.Fatal(err)
	}
	defer el.Close()

	// Emit 3 events before subscribing.
	el.Emit(ProcessEvent{Type: EventSpawned, PID: 1})
	el.Emit(ProcessEvent{Type: EventSpawned, PID: 2})
	el.Emit(ProcessEvent{Type: EventStateChanged, PID: 1, NewState: "running"})

	// Subscribe since seq 1 (should replay seq 2 and 3, then get live).
	ch := el.SubscribeSince(1, 64)

	// Read replayed events.
	got := make([]ProcessEvent, 0, 2)
	for i := 0; i < 2; i++ {
		select {
		case e := <-ch:
			got = append(got, e)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for replayed event")
		}
	}

	if got[0].Seq != 2 || got[1].Seq != 3 {
		t.Fatalf("replayed seqs: expected 2,3; got %d,%d", got[0].Seq, got[1].Seq)
	}

	// Now emit a live event.
	el.Emit(ProcessEvent{Type: EventRemoved, PID: 2})

	select {
	case e := <-ch:
		if e.Seq != 4 {
			t.Fatalf("live event seq: expected 4, got %d", e.Seq)
		}
		if e.Type != EventRemoved {
			t.Fatalf("live event type: expected removed, got %s", e.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for live event")
	}

	el.Unsubscribe(ch)
}

func TestRingBufferTrim(t *testing.T) {
	el, err := NewEventLog(10, "")
	if err != nil {
		t.Fatal(err)
	}
	defer el.Close()

	// Emit 15 events into a buffer of size 10.
	for i := 0; i < 15; i++ {
		el.Emit(ProcessEvent{Type: EventSpawned, PID: uint64(i + 1)})
	}

	if el.CurrentSeq() != 15 {
		t.Fatalf("expected seq 15, got %d", el.CurrentSeq())
	}

	// Buffer should have been trimmed. Since(0) returns whatever is in the buffer.
	evts := el.Since(0)
	if len(evts) > 10 {
		t.Fatalf("expected at most 10 buffered events, got %d", len(evts))
	}

	// All returned events should have seq > 0 and be valid.
	for _, e := range evts {
		if e.Seq == 0 {
			t.Fatal("event has seq 0")
		}
	}

	// The most recent event should be seq 15.
	last := evts[len(evts)-1]
	if last.Seq != 15 {
		t.Fatalf("last event seq: expected 15, got %d", last.Seq)
	}
}

func TestDiskPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	el, err := NewEventLog(100, path)
	if err != nil {
		t.Fatal(err)
	}

	el.Emit(ProcessEvent{Type: EventSpawned, PID: 1, Name: "king"})
	el.Emit(ProcessEvent{Type: EventStateChanged, PID: 1, OldState: "idle", NewState: "running"})
	el.Emit(ProcessEvent{Type: EventRemoved, PID: 1})
	el.Close()

	// Read back the file.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 JSONL lines, got %d", len(lines))
	}

	// Verify first line contains "spawned" and "king".
	if !strings.Contains(lines[0], `"spawned"`) || !strings.Contains(lines[0], `"king"`) {
		t.Fatalf("first line should contain spawned+king: %s", lines[0])
	}

	// Verify second line contains state_changed.
	if !strings.Contains(lines[1], `"state_changed"`) {
		t.Fatalf("second line should contain state_changed: %s", lines[1])
	}
}

func TestRegistryHooks(t *testing.T) {
	el, err := NewEventLog(100, "")
	if err != nil {
		t.Fatal(err)
	}
	defer el.Close()

	r := NewRegistry()
	r.SetEventLog(el)

	ch := el.SubscribeSince(0, 64)

	// Register should emit EventSpawned.
	p := &Process{Name: "test-agent", Role: RoleWorker, CognitiveTier: CogOperational, Model: "mini"}
	r.Register(p)

	e := <-ch
	if e.Type != EventSpawned {
		t.Fatalf("expected spawned, got %s", e.Type)
	}
	if e.PID != p.PID {
		t.Fatalf("expected PID %d, got %d", p.PID, e.PID)
	}
	if e.Name != "test-agent" {
		t.Fatalf("expected name test-agent, got %s", e.Name)
	}
	if e.Role != "worker" {
		t.Fatalf("expected role worker, got %s", e.Role)
	}

	// SetState should emit EventStateChanged.
	r.SetState(p.PID, StateRunning)

	e = <-ch
	if e.Type != EventStateChanged {
		t.Fatalf("expected state_changed, got %s", e.Type)
	}
	if e.OldState != "idle" {
		t.Fatalf("expected old_state idle, got %s", e.OldState)
	}
	if e.NewState != "running" {
		t.Fatalf("expected new_state running, got %s", e.NewState)
	}

	// Remove should emit EventRemoved.
	r.Remove(p.PID)

	e = <-ch
	if e.Type != EventRemoved {
		t.Fatalf("expected removed, got %s", e.Type)
	}
	if e.PID != p.PID {
		t.Fatalf("expected PID %d, got %d", p.PID, e.PID)
	}

	el.Unsubscribe(ch)
}

func TestNoEventOnSameState(t *testing.T) {
	el, err := NewEventLog(100, "")
	if err != nil {
		t.Fatal(err)
	}
	defer el.Close()

	r := NewRegistry()
	r.SetEventLog(el)

	p := &Process{Name: "x", State: StateIdle}
	r.Register(p)

	seqBefore := el.CurrentSeq()

	// SetState to the same state should not emit.
	r.SetState(p.PID, StateIdle)

	if el.CurrentSeq() != seqBefore {
		t.Fatalf("expected no event on same state, seq changed from %d to %d", seqBefore, el.CurrentSeq())
	}
}

func TestConcurrency(t *testing.T) {
	el, err := NewEventLog(1000, "")
	if err != nil {
		t.Fatal(err)
	}
	defer el.Close()

	const numEmitters = 10
	const eventsPerEmitter = 100

	ch := el.SubscribeSince(0, numEmitters*eventsPerEmitter+100)

	var wg sync.WaitGroup
	wg.Add(numEmitters)
	for i := 0; i < numEmitters; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < eventsPerEmitter; j++ {
				el.Emit(ProcessEvent{
					Type: EventSpawned,
					PID:  uint64(id*1000 + j),
				})
			}
		}(i)
	}

	wg.Wait()

	expected := uint64(numEmitters * eventsPerEmitter)
	if el.CurrentSeq() != expected {
		t.Fatalf("expected seq %d, got %d", expected, el.CurrentSeq())
	}

	// Drain subscriber channel and count.
	received := 0
	for {
		select {
		case <-ch:
			received++
		default:
			goto done
		}
	}
done:
	if received != int(expected) {
		t.Fatalf("subscriber received %d events, expected %d", received, expected)
	}

	el.Unsubscribe(ch)
}
