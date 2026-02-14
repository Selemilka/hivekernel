package process

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Registry is a thread-safe process table.
// It stores all processes and provides CRUD + tree operations.
type Registry struct {
	mu        sync.RWMutex
	processes map[PID]*Process
	byName    map[string]PID // name → PID for quick lookup
	nextPID   atomic.Uint64
	eventLog  *EventLog
}

// SetEventLog wires an EventLog so that Register/SetState/Remove emit events.
func (r *Registry) SetEventLog(el *EventLog) {
	r.eventLog = el
}

// NewRegistry creates an empty process registry.
func NewRegistry() *Registry {
	r := &Registry{
		processes: make(map[PID]*Process),
		byName:    make(map[string]PID),
	}
	// PID 0 is reserved (means "self" in GetProcessInfo), start from 1.
	r.nextPID.Store(1)
	return r
}

// allocPID returns the next available PID.
func (r *Registry) allocPID() PID {
	return r.nextPID.Add(1) - 1
}

// Register adds a new process to the table and returns its assigned PID.
// The Process.PID field is set by the registry.
func (r *Registry) Register(p *Process) (PID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	pid := r.allocPID()
	p.PID = pid
	p.StartedAt = time.Now()
	p.UpdatedAt = time.Now()

	r.processes[pid] = p
	if p.Name != "" {
		r.byName[p.Name] = pid
	}

	if r.eventLog != nil {
		r.eventLog.Emit(ProcessEvent{
			Type:  EventSpawned,
			PID:   pid,
			PPID:  p.PPID,
			Name:  p.Name,
			Role:  p.Role.String(),
			Tier:  p.CognitiveTier.String(),
			Model: p.Model,
			State: p.State.String(),
		})
	}

	return pid, nil
}

// Get returns a process by PID, or an error if not found.
func (r *Registry) Get(pid PID) (*Process, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	p, ok := r.processes[pid]
	if !ok {
		return nil, fmt.Errorf("process %d not found", pid)
	}
	return p, nil
}

// GetByName returns a process by name.
func (r *Registry) GetByName(name string) (*Process, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	pid, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("process %q not found", name)
	}
	p, ok := r.processes[pid]
	if !ok {
		return nil, fmt.Errorf("process %q (PID %d) not in table", name, pid)
	}
	return p, nil
}

// Update modifies a process in-place using the provided function.
// Returns an error if the PID doesn't exist.
func (r *Registry) Update(pid PID, fn func(*Process)) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	p, ok := r.processes[pid]
	if !ok {
		return fmt.Errorf("process %d not found", pid)
	}
	fn(p)
	p.UpdatedAt = time.Now()
	return nil
}

// SetState updates a process's state and emits a state_changed event if the
// state actually changed.
func (r *Registry) SetState(pid PID, state ProcessState) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	p, ok := r.processes[pid]
	if !ok {
		return fmt.Errorf("process %d not found", pid)
	}
	oldState := p.State
	p.State = state
	p.UpdatedAt = time.Now()

	if r.eventLog != nil && oldState != state {
		r.eventLog.Emit(ProcessEvent{
			Type:     EventStateChanged,
			PID:      pid,
			OldState: oldState.String(),
			NewState: state.String(),
		})
	}
	return nil
}

// Remove deletes a process from the table.
func (r *Registry) Remove(pid PID) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	p, ok := r.processes[pid]
	if !ok {
		return fmt.Errorf("process %d not found", pid)
	}
	if p.Name != "" {
		delete(r.byName, p.Name)
	}
	delete(r.processes, pid)

	if r.eventLog != nil {
		r.eventLog.Emit(ProcessEvent{
			Type: EventRemoved,
			PID:  pid,
		})
	}

	return nil
}

// GetChildren returns all direct children of a process.
func (r *Registry) GetChildren(ppid PID) []*Process {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var children []*Process
	for _, p := range r.processes {
		if p.PPID == ppid && p.PID != ppid {
			children = append(children, p)
		}
	}
	return children
}

// GetDescendants returns all descendants of a process (recursive).
func (r *Registry) GetDescendants(pid PID) []*Process {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*Process
	r.collectDescendants(pid, &result)
	return result
}

func (r *Registry) collectDescendants(pid PID, result *[]*Process) {
	for _, p := range r.processes {
		if p.PPID == pid && p.PID != pid {
			*result = append(*result, p)
			r.collectDescendants(p.PID, result)
		}
	}
}

// CountChildren returns the number of direct children.
func (r *Registry) CountChildren(ppid PID) int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	count := 0
	for _, p := range r.processes {
		if p.PPID == ppid && p.PID != ppid {
			count++
		}
	}
	return count
}

// List returns all processes. The caller must not modify the returned slice elements.
func (r *Registry) List() []*Process {
	r.mu.RLock()
	defer r.mu.RUnlock()

	list := make([]*Process, 0, len(r.processes))
	for _, p := range r.processes {
		list = append(list, p)
	}
	return list
}

// FindAncestor walks up from pid and returns the first ancestor matching the predicate.
func (r *Registry) FindAncestor(pid PID, match func(*Process) bool) (*Process, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	current, ok := r.processes[pid]
	if !ok {
		return nil, false
	}
	for current.PPID != 0 && current.PPID != current.PID {
		parent, ok := r.processes[current.PPID]
		if !ok {
			return nil, false
		}
		if match(parent) {
			return parent, true
		}
		current = parent
	}
	return nil, false
}

// NearestCommonAncestor finds the nearest common ancestor of two PIDs.
func (r *Registry) NearestCommonAncestor(a, b PID) (PID, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Collect ancestors of a.
	ancestors := make(map[PID]struct{})
	cur := a
	for {
		ancestors[cur] = struct{}{}
		p, ok := r.processes[cur]
		if !ok || p.PPID == cur || p.PPID == 0 {
			break
		}
		cur = p.PPID
	}

	// Walk up from b, first match is NCA.
	cur = b
	for {
		if _, ok := ancestors[cur]; ok {
			return cur, true
		}
		p, ok := r.processes[cur]
		if !ok || p.PPID == cur || p.PPID == 0 {
			break
		}
		cur = p.PPID
	}
	return 0, false
}
