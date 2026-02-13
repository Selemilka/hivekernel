package cluster

import (
	"fmt"
	"sync"
	"time"
)

// NodeStatus represents the current state of a VPS node.
type NodeStatus int

const (
	NodeOnline     NodeStatus = iota // healthy, accepting processes
	NodeOverloaded                   // above resource thresholds
	NodeDraining                     // migration in progress, no new processes
	NodeOffline                      // unreachable
)

func (s NodeStatus) String() string {
	switch s {
	case NodeOnline:
		return "online"
	case NodeOverloaded:
		return "overloaded"
	case NodeDraining:
		return "draining"
	case NodeOffline:
		return "offline"
	default:
		return "unknown"
	}
}

// NodeInfo describes a VPS node in the cluster.
type NodeInfo struct {
	ID      string // e.g. "vps1", "vps2"
	Address string // gRPC address, e.g. "10.0.0.2:50051"

	// Capacity
	Cores    int    // CPU cores
	MemoryMB uint64 // total RAM in MB
	DiskMB   uint64 // total disk in MB

	// Current utilization
	MemoryUsedMB uint64
	DiskUsedMB   uint64
	ProcessCount int // active processes on this node

	// Health
	Status        NodeStatus
	LastHeartbeat time.Time
	RegisteredAt  time.Time
}

// MemoryFreePercent returns percentage of free memory.
func (n *NodeInfo) MemoryFreePercent() float64 {
	if n.MemoryMB == 0 {
		return 0
	}
	return float64(n.MemoryMB-n.MemoryUsedMB) / float64(n.MemoryMB) * 100
}

// LoadScore returns a composite load score (lower = less loaded).
// Used by FindLeastLoaded to pick migration targets.
func (n *NodeInfo) LoadScore() float64 {
	if n.MemoryMB == 0 {
		return 100
	}
	memUsage := float64(n.MemoryUsedMB) / float64(n.MemoryMB)
	processWeight := float64(n.ProcessCount) * 0.01
	return memUsage + processWeight
}

// NodeRegistry tracks all VPS nodes in the cluster.
type NodeRegistry struct {
	mu    sync.RWMutex
	nodes map[string]*NodeInfo // nodeID -> info
}

// NewNodeRegistry creates an empty node registry.
func NewNodeRegistry() *NodeRegistry {
	return &NodeRegistry{
		nodes: make(map[string]*NodeInfo),
	}
}

// Register adds or updates a VPS node.
func (r *NodeRegistry) Register(node *NodeInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if node.RegisteredAt.IsZero() {
		node.RegisteredAt = time.Now()
	}
	node.LastHeartbeat = time.Now()
	if node.Status == NodeOffline {
		node.Status = NodeOnline
	}
	r.nodes[node.ID] = node
}

// Deregister removes a node from the registry.
func (r *NodeRegistry) Deregister(nodeID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.nodes[nodeID]; !ok {
		return fmt.Errorf("node %q not found", nodeID)
	}
	delete(r.nodes, nodeID)
	return nil
}

// Get returns a node by ID.
func (r *NodeRegistry) Get(nodeID string) (*NodeInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	node, ok := r.nodes[nodeID]
	if !ok {
		return nil, fmt.Errorf("node %q not found", nodeID)
	}
	return node, nil
}

// List returns all registered nodes.
func (r *NodeRegistry) List() []*NodeInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	list := make([]*NodeInfo, 0, len(r.nodes))
	for _, n := range r.nodes {
		list = append(list, n)
	}
	return list
}

// ListOnline returns all nodes that are online (not offline/draining).
func (r *NodeRegistry) ListOnline() []*NodeInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var list []*NodeInfo
	for _, n := range r.nodes {
		if n.Status == NodeOnline {
			list = append(list, n)
		}
	}
	return list
}

// UpdateHealth updates a node's health metrics.
func (r *NodeRegistry) UpdateHealth(nodeID string, memUsedMB, diskUsedMB uint64, processCount int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	node, ok := r.nodes[nodeID]
	if !ok {
		return fmt.Errorf("node %q not found", nodeID)
	}

	node.MemoryUsedMB = memUsedMB
	node.DiskUsedMB = diskUsedMB
	node.ProcessCount = processCount
	node.LastHeartbeat = time.Now()

	// Auto-detect overloaded state: >90% memory.
	if node.MemoryMB > 0 && float64(memUsedMB)/float64(node.MemoryMB) > 0.9 {
		node.Status = NodeOverloaded
	} else if node.Status == NodeOverloaded {
		// Recovered from overloaded.
		node.Status = NodeOnline
	}

	return nil
}

// SetStatus explicitly sets a node's status.
func (r *NodeRegistry) SetStatus(nodeID string, status NodeStatus) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	node, ok := r.nodes[nodeID]
	if !ok {
		return fmt.Errorf("node %q not found", nodeID)
	}
	node.Status = status
	return nil
}

// Heartbeat updates the last heartbeat timestamp for a node.
func (r *NodeRegistry) Heartbeat(nodeID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	node, ok := r.nodes[nodeID]
	if !ok {
		return fmt.Errorf("node %q not found", nodeID)
	}
	node.LastHeartbeat = time.Now()
	return nil
}

// FindLeastLoaded returns the online node with the lowest load score,
// excluding the specified node IDs.
func (r *NodeRegistry) FindLeastLoaded(exclude ...string) (*NodeInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	excludeSet := make(map[string]struct{}, len(exclude))
	for _, id := range exclude {
		excludeSet[id] = struct{}{}
	}

	var best *NodeInfo
	bestScore := float64(999)

	for _, n := range r.nodes {
		if n.Status != NodeOnline {
			continue
		}
		if _, excluded := excludeSet[n.ID]; excluded {
			continue
		}
		score := n.LoadScore()
		if score < bestScore {
			bestScore = score
			best = n
		}
	}

	if best == nil {
		return nil, fmt.Errorf("no available online nodes")
	}
	return best, nil
}

// CheckStale marks nodes as offline if they haven't sent a heartbeat
// within the given timeout duration.
func (r *NodeRegistry) CheckStale(timeout time.Duration) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var stale []string
	cutoff := time.Now().Add(-timeout)

	for _, n := range r.nodes {
		if n.Status != NodeOffline && n.LastHeartbeat.Before(cutoff) {
			n.Status = NodeOffline
			stale = append(stale, n.ID)
		}
	}
	return stale
}

// Count returns the number of registered nodes.
func (r *NodeRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.nodes)
}
