package ipc

import (
	"fmt"
	"sync"

	"github.com/selemilka/hivekernel/internal/process"
)

// Pipe is a bidirectional byte channel between parent and child.
// Modeled after Unix pipes but typed and with flow control.
type Pipe struct {
	ParentPID process.PID
	ChildPID  process.PID

	// Channels for bidirectional communication.
	parentToChild chan []byte
	childToParent chan []byte

	closed bool
	mu     sync.Mutex
}

// NewPipe creates a bidirectional pipe between parent and child.
func NewPipe(parentPID, childPID process.PID, bufferSize int) *Pipe {
	if bufferSize <= 0 {
		bufferSize = 64
	}
	return &Pipe{
		ParentPID:     parentPID,
		ChildPID:      childPID,
		parentToChild: make(chan []byte, bufferSize),
		childToParent: make(chan []byte, bufferSize),
	}
}

// WriteToChild sends data from parent to child. Non-blocking; returns error if full.
func (p *Pipe) WriteToChild(data []byte) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return fmt.Errorf("pipe closed")
	}
	p.mu.Unlock()

	select {
	case p.parentToChild <- data:
		return nil
	default:
		return fmt.Errorf("pipe to child full (PID %d -> %d)", p.ParentPID, p.ChildPID)
	}
}

// WriteToParent sends data from child to parent. Non-blocking; returns error if full.
func (p *Pipe) WriteToParent(data []byte) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return fmt.Errorf("pipe closed")
	}
	p.mu.Unlock()

	select {
	case p.childToParent <- data:
		return nil
	default:
		return fmt.Errorf("pipe to parent full (PID %d -> %d)", p.ChildPID, p.ParentPID)
	}
}

// ReadFromParent returns the channel a child reads from.
func (p *Pipe) ReadFromParent() <-chan []byte {
	return p.parentToChild
}

// ReadFromChild returns the channel a parent reads from.
func (p *Pipe) ReadFromChild() <-chan []byte {
	return p.childToParent
}

// Close shuts down the pipe in both directions.
func (p *Pipe) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		close(p.parentToChild)
		close(p.childToParent)
	}
}

// IsClosed returns whether the pipe has been closed.
func (p *Pipe) IsClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// PipeRegistry manages pipes between parent-child pairs.
type PipeRegistry struct {
	mu    sync.RWMutex
	pipes map[pipeKey]*Pipe
}

type pipeKey struct {
	parent, child process.PID
}

// NewPipeRegistry creates a new pipe registry.
func NewPipeRegistry() *PipeRegistry {
	return &PipeRegistry{
		pipes: make(map[pipeKey]*Pipe),
	}
}

// Create creates a new pipe between parent and child.
func (pr *PipeRegistry) Create(parentPID, childPID process.PID, bufferSize int) *Pipe {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	key := pipeKey{parentPID, childPID}
	if p, ok := pr.pipes[key]; ok {
		return p
	}
	p := NewPipe(parentPID, childPID, bufferSize)
	pr.pipes[key] = p
	return p
}

// Get returns the pipe for a parent-child pair.
func (pr *PipeRegistry) Get(parentPID, childPID process.PID) (*Pipe, bool) {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	p, ok := pr.pipes[pipeKey{parentPID, childPID}]
	return p, ok
}

// Remove closes and removes a pipe.
func (pr *PipeRegistry) Remove(parentPID, childPID process.PID) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	key := pipeKey{parentPID, childPID}
	if p, ok := pr.pipes[key]; ok {
		p.Close()
		delete(pr.pipes, key)
	}
}
