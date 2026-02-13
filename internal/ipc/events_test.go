package ipc

import (
	"testing"
	"time"
)

func TestEventBusPubSub(t *testing.T) {
	eb := NewEventBus()

	ch := eb.Subscribe("test_topic", 10)

	eb.Publish("test_topic", 42, []byte("hello"))

	select {
	case evt := <-ch:
		if evt.Topic != "test_topic" {
			t.Fatalf("expected topic test_topic, got %s", evt.Topic)
		}
		if evt.Source != 42 {
			t.Fatalf("expected source 42, got %d", evt.Source)
		}
		if string(evt.Payload) != "hello" {
			t.Fatalf("expected payload 'hello', got %q", string(evt.Payload))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("did not receive event")
	}
}

func TestEventBusMultipleSubscribers(t *testing.T) {
	eb := NewEventBus()

	ch1 := eb.Subscribe("topic", 10)
	ch2 := eb.Subscribe("topic", 10)

	eb.Publish("topic", 1, []byte("data"))

	for i, ch := range []chan Event{ch1, ch2} {
		select {
		case evt := <-ch:
			if string(evt.Payload) != "data" {
				t.Fatalf("subscriber %d got wrong payload", i)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("subscriber %d did not receive event", i)
		}
	}
}

func TestEventBusUnsubscribe(t *testing.T) {
	eb := NewEventBus()

	ch := eb.Subscribe("topic", 10)
	eb.Unsubscribe("topic", ch)

	if eb.TopicCount("topic") != 0 {
		t.Fatal("expected 0 subscribers after unsubscribe")
	}
}

func TestEventBusNoSubscribers(t *testing.T) {
	eb := NewEventBus()

	// Should not panic even with no subscribers.
	eb.Publish("nobody_listens", 1, []byte("void"))
}

func TestEventBusTopicIsolation(t *testing.T) {
	eb := NewEventBus()

	chA := eb.Subscribe("topicA", 10)
	chB := eb.Subscribe("topicB", 10)

	eb.Publish("topicA", 1, []byte("for A"))

	select {
	case <-chA:
		// OK
	case <-time.After(50 * time.Millisecond):
		t.Fatal("topicA subscriber should receive event")
	}

	select {
	case <-chB:
		t.Fatal("topicB subscriber should NOT receive topicA event")
	case <-time.After(50 * time.Millisecond):
		// OK - no event for topicB
	}
}
