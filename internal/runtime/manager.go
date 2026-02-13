package runtime

import (
	"fmt"
	"log"
	"sync"

	"github.com/selemilka/hivekernel/internal/process"
)

// RuntimeType identifies which runtime to use for an agent.
type RuntimeType string

const (
	RuntimePython RuntimeType = "python"
	RuntimeClaw   RuntimeType = "claw"
	RuntimeCustom RuntimeType = "custom"
)

// AgentRuntime represents a running agent runtime process.
type AgentRuntime struct {
	PID         process.PID
	Type        RuntimeType
	Addr        string // gRPC address (unix socket or tcp)
	ProcessInfo *process.Process
}

// Manager manages agent runtime processes (OS process spawn + gRPC connection).
type Manager struct {
	mu       sync.RWMutex
	runtimes map[process.PID]*AgentRuntime
}

// NewManager creates a new runtime manager.
func NewManager() *Manager {
	return &Manager{
		runtimes: make(map[process.PID]*AgentRuntime),
	}
}

// StartRuntime launches an agent runtime for the given process.
// Phase 0: registers the runtime entry but doesn't actually start an OS process yet.
// The real implementation will exec a Python process and connect via gRPC.
func (m *Manager) StartRuntime(proc *process.Process, rtType RuntimeType) (*AgentRuntime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.runtimes[proc.PID]; exists {
		return nil, fmt.Errorf("runtime already running for PID %d", proc.PID)
	}

	// Generate socket path for this agent.
	addr := fmt.Sprintf("unix:///tmp/hivekernel-agent-%d.sock", proc.PID)

	rt := &AgentRuntime{
		PID:         proc.PID,
		Type:        rtType,
		Addr:        addr,
		ProcessInfo: proc,
	}
	m.runtimes[proc.PID] = rt

	// Update process with runtime address.
	proc.RuntimeAddr = addr

	log.Printf("[runtime] registered runtime for PID %d (%s) at %s", proc.PID, proc.Name, addr)

	// TODO(phase0): actually exec the agent process.
	// For now, the runtime is just registered. Real implementation:
	//   1. exec python -m hivekernel_sdk.runner --socket <addr> --pid <pid>
	//   2. wait for agent to bind the socket
	//   3. connect gRPC client, call Init()

	return rt, nil
}

// StopRuntime terminates an agent runtime.
func (m *Manager) StopRuntime(pid process.PID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rt, ok := m.runtimes[pid]
	if !ok {
		return fmt.Errorf("no runtime for PID %d", pid)
	}

	log.Printf("[runtime] stopping runtime for PID %d (%s)", pid, rt.ProcessInfo.Name)

	// TODO(phase0): send Shutdown RPC, then kill OS process.
	delete(m.runtimes, pid)
	return nil
}

// GetRuntime returns the runtime for a PID, or nil.
func (m *Manager) GetRuntime(pid process.PID) *AgentRuntime {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.runtimes[pid]
}

// ListRuntimes returns all active runtimes.
func (m *Manager) ListRuntimes() []*AgentRuntime {
	m.mu.RLock()
	defer m.mu.RUnlock()

	list := make([]*AgentRuntime, 0, len(m.runtimes))
	for _, rt := range m.runtimes {
		list = append(list, rt)
	}
	return list
}
