package ipc

import (
	"testing"
)

func TestPipeBidirectional(t *testing.T) {
	p := NewPipe(1, 2, 4)

	// Parent writes, child reads.
	if err := p.WriteToChild([]byte("hello child")); err != nil {
		t.Fatalf("WriteToChild: %v", err)
	}
	data := <-p.ReadFromParent()
	if string(data) != "hello child" {
		t.Fatalf("expected 'hello child', got %q", string(data))
	}

	// Child writes, parent reads.
	if err := p.WriteToParent([]byte("hello parent")); err != nil {
		t.Fatalf("WriteToParent: %v", err)
	}
	data = <-p.ReadFromChild()
	if string(data) != "hello parent" {
		t.Fatalf("expected 'hello parent', got %q", string(data))
	}
}

func TestPipeClose(t *testing.T) {
	p := NewPipe(1, 2, 4)
	p.Close()

	if !p.IsClosed() {
		t.Fatal("pipe should be closed")
	}

	err := p.WriteToChild([]byte("after close"))
	if err == nil {
		t.Fatal("write after close should fail")
	}
}

func TestPipeRegistryCreateGet(t *testing.T) {
	pr := NewPipeRegistry()

	p := pr.Create(1, 2, 8)
	if p == nil {
		t.Fatal("Create returned nil")
	}

	got, ok := pr.Get(1, 2)
	if !ok || got != p {
		t.Fatal("Get should return the same pipe")
	}

	// Duplicate Create returns same pipe.
	p2 := pr.Create(1, 2, 8)
	if p2 != p {
		t.Fatal("duplicate Create should return same pipe")
	}
}

func TestPipeRegistryRemove(t *testing.T) {
	pr := NewPipeRegistry()
	p := pr.Create(1, 2, 8)

	pr.Remove(1, 2)

	if !p.IsClosed() {
		t.Fatal("pipe should be closed after Remove")
	}

	_, ok := pr.Get(1, 2)
	if ok {
		t.Fatal("pipe should not exist after Remove")
	}
}

func TestPipeBackpressure(t *testing.T) {
	p := NewPipe(1, 2, 2) // buffer size 2

	// Fill buffer.
	p.WriteToChild([]byte("1"))
	p.WriteToChild([]byte("2"))

	// Third write should fail (non-blocking).
	err := p.WriteToChild([]byte("3"))
	if err == nil {
		t.Fatal("expected backpressure error when pipe is full")
	}
}
