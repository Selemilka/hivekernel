package resources

import (
	"fmt"
	"sync"

	"github.com/selemilka/hivekernel/internal/process"
)

// CGroup defines collective resource limits for a branch of the process tree.
// Named after Linux cgroups — constrains a group of processes as a unit.
type CGroup struct {
	Name    string
	RootPID process.PID // root of the branch this group covers

	// Limits
	MaxTokensTotal uint64 // total tokens across all processes in the group
	MaxProcesses   int    // max concurrent processes in the group

	// Tracked usage
	TokensConsumed uint64
	ProcessCount   int

	// Members
	members map[process.PID]struct{}
}

// Remaining returns tokens remaining for the group.
func (g *CGroup) Remaining() uint64 {
	if g.MaxTokensTotal == 0 {
		return 0 // unlimited, tracking only
	}
	if g.TokensConsumed >= g.MaxTokensTotal {
		return 0
	}
	return g.MaxTokensTotal - g.TokensConsumed
}

// UsagePercent returns the percentage of token budget used.
func (g *CGroup) UsagePercent() float64 {
	if g.MaxTokensTotal == 0 {
		return 0
	}
	return float64(g.TokensConsumed) / float64(g.MaxTokensTotal) * 100
}

// Members returns a copy of the member PID set.
func (g *CGroup) Members() []process.PID {
	pids := make([]process.PID, 0, len(g.members))
	for pid := range g.members {
		pids = append(pids, pid)
	}
	return pids
}

// CGroupManager manages process groups with collective resource limits.
type CGroupManager struct {
	mu       sync.RWMutex
	groups   map[string]*CGroup      // name -> group
	pidGroup map[process.PID]string   // pid -> group name (each process in at most one group)
	registry *process.Registry
}

// NewCGroupManager creates a new cgroup manager.
func NewCGroupManager(registry *process.Registry) *CGroupManager {
	return &CGroupManager{
		groups:   make(map[string]*CGroup),
		pidGroup: make(map[process.PID]string),
		registry: registry,
	}
}

// Create creates a new cgroup rooted at the given PID.
func (m *CGroupManager) Create(name string, rootPID process.PID, maxTokens uint64, maxProcs int) (*CGroup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.groups[name]; exists {
		return nil, fmt.Errorf("cgroup %q already exists", name)
	}

	// Validate root process exists.
	if _, err := m.registry.Get(rootPID); err != nil {
		return nil, fmt.Errorf("root process: %w", err)
	}

	g := &CGroup{
		Name:           name,
		RootPID:        rootPID,
		MaxTokensTotal: maxTokens,
		MaxProcesses:   maxProcs,
		members:        make(map[process.PID]struct{}),
	}

	// Auto-add root and all descendants.
	g.members[rootPID] = struct{}{}
	m.pidGroup[rootPID] = name
	g.ProcessCount = 1

	descendants := m.registry.GetDescendants(rootPID)
	for _, d := range descendants {
		g.members[d.PID] = struct{}{}
		m.pidGroup[d.PID] = name
		g.ProcessCount++
	}

	m.groups[name] = g
	return g, nil
}

// Delete removes a cgroup and all process associations.
func (m *CGroupManager) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	g, ok := m.groups[name]
	if !ok {
		return fmt.Errorf("cgroup %q not found", name)
	}

	for pid := range g.members {
		delete(m.pidGroup, pid)
	}
	delete(m.groups, name)
	return nil
}

// Get returns a cgroup by name.
func (m *CGroupManager) Get(name string) (*CGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	g, ok := m.groups[name]
	if !ok {
		return nil, fmt.Errorf("cgroup %q not found", name)
	}
	return g, nil
}

// GetByPID returns the cgroup a process belongs to (if any).
func (m *CGroupManager) GetByPID(pid process.PID) (*CGroup, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	name, ok := m.pidGroup[pid]
	if !ok {
		return nil, false
	}
	g, ok := m.groups[name]
	return g, ok
}

// List returns all cgroups.
func (m *CGroupManager) List() []*CGroup {
	m.mu.RLock()
	defer m.mu.RUnlock()

	list := make([]*CGroup, 0, len(m.groups))
	for _, g := range m.groups {
		list = append(list, g)
	}
	return list
}

// AddProcess adds a process to a cgroup.
func (m *CGroupManager) AddProcess(name string, pid process.PID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	g, ok := m.groups[name]
	if !ok {
		return fmt.Errorf("cgroup %q not found", name)
	}

	if _, already := g.members[pid]; already {
		return nil // idempotent
	}

	// Check max processes limit.
	if g.MaxProcesses > 0 && g.ProcessCount >= g.MaxProcesses {
		return fmt.Errorf("cgroup %q at max processes (%d)", name, g.MaxProcesses)
	}

	// Remove from old group if any.
	if oldName, inOld := m.pidGroup[pid]; inOld {
		if old, ok := m.groups[oldName]; ok {
			delete(old.members, pid)
			old.ProcessCount--
		}
	}

	g.members[pid] = struct{}{}
	g.ProcessCount++
	m.pidGroup[pid] = name
	return nil
}

// RemoveProcess removes a process from its cgroup.
func (m *CGroupManager) RemoveProcess(pid process.PID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	name, ok := m.pidGroup[pid]
	if !ok {
		return
	}

	if g, ok := m.groups[name]; ok {
		delete(g.members, pid)
		g.ProcessCount--
	}
	delete(m.pidGroup, pid)
}

// ConsumeTokens records token consumption against the group's collective budget.
// Returns an error if the group would exceed its limit.
func (m *CGroupManager) ConsumeTokens(pid process.PID, tokens uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	name, ok := m.pidGroup[pid]
	if !ok {
		return nil // process not in any group, no group limit applies
	}

	g, ok := m.groups[name]
	if !ok {
		return nil
	}

	if g.MaxTokensTotal > 0 && g.TokensConsumed+tokens > g.MaxTokensTotal {
		return fmt.Errorf("cgroup %q token limit exceeded: consumed=%d + request=%d > max=%d",
			name, g.TokensConsumed, tokens, g.MaxTokensTotal)
	}

	g.TokensConsumed += tokens
	return nil
}

// CheckSpawnAllowed checks whether a new process can be spawned in a group.
func (m *CGroupManager) CheckSpawnAllowed(parentPID process.PID) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	name, ok := m.pidGroup[parentPID]
	if !ok {
		return nil // parent not in any group
	}

	g, ok := m.groups[name]
	if !ok {
		return nil
	}

	if g.MaxProcesses > 0 && g.ProcessCount >= g.MaxProcesses {
		return fmt.Errorf("cgroup %q at max processes (%d/%d)", name, g.ProcessCount, g.MaxProcesses)
	}
	return nil
}

// GetGroupUsage returns aggregate usage for a cgroup.
type GroupUsage struct {
	Name           string
	RootPID        process.PID
	TokensConsumed uint64
	TokensMax      uint64
	ProcessCount   int
	ProcessMax     int
	UsagePercent   float64
}

// GetGroupUsage returns usage summary for a cgroup.
func (m *CGroupManager) GetGroupUsage(name string) (*GroupUsage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	g, ok := m.groups[name]
	if !ok {
		return nil, fmt.Errorf("cgroup %q not found", name)
	}

	return &GroupUsage{
		Name:           g.Name,
		RootPID:        g.RootPID,
		TokensConsumed: g.TokensConsumed,
		TokensMax:      g.MaxTokensTotal,
		ProcessCount:   g.ProcessCount,
		ProcessMax:     g.MaxProcesses,
		UsagePercent:   g.UsagePercent(),
	}, nil
}
