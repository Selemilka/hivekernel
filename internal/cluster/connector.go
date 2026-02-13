package cluster

import (
	"fmt"
	"sync"
	"time"
)

// ConnState represents the state of a VPS connection.
type ConnState int

const (
	ConnIdle        ConnState = iota // registered but not connected
	ConnConnecting                   // connection attempt in progress
	ConnReady                        // connected and healthy
	ConnUnhealthy                    // connected but failing health checks
	ConnClosed                       // explicitly closed
)

func (s ConnState) String() string {
	switch s {
	case ConnIdle:
		return "idle"
	case ConnConnecting:
		return "connecting"
	case ConnReady:
		return "ready"
	case ConnUnhealthy:
		return "unhealthy"
	case ConnClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// VPSConnection represents a connection to a remote VPS node.
// In production this would wrap a gRPC ClientConn, but the interface
// is kept transport-agnostic so the core logic is testable without real gRPC.
type VPSConnection struct {
	NodeID    string
	Address   string
	State     ConnState
	CreatedAt time.Time
	LastUsed  time.Time

	// MessagesSent tracks how many messages were forwarded through this connection.
	MessagesSent uint64
	// Errors tracks consecutive connection errors (resets on success).
	Errors int
}

// ForwardedMessage represents a message being forwarded to a remote VPS.
type ForwardedMessage struct {
	ID      string
	FromPID uint64
	ToPID   uint64
	ToQueue string
	Type    string
	Payload []byte
}

// Connector manages connections to remote VPS nodes.
// It provides a connection pool with health tracking and message forwarding.
type Connector struct {
	mu          sync.RWMutex
	connections map[string]*VPSConnection // nodeID -> connection
	forwarded   []ForwardedMessage        // log of forwarded messages (for testing/audit)

	maxErrors int // consecutive errors before marking unhealthy
}

// NewConnector creates a new connector with default settings.
func NewConnector() *Connector {
	return &Connector{
		connections: make(map[string]*VPSConnection),
		maxErrors:   3,
	}
}

// Connect registers a connection to a remote VPS node.
// If a connection already exists, it updates the address and resets the state.
func (c *Connector) Connect(nodeID, address string) (*VPSConnection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if nodeID == "" {
		return nil, fmt.Errorf("node ID is required")
	}
	if address == "" {
		return nil, fmt.Errorf("address is required")
	}

	conn := &VPSConnection{
		NodeID:    nodeID,
		Address:   address,
		State:     ConnReady,
		CreatedAt: time.Now(),
		LastUsed:  time.Now(),
	}
	c.connections[nodeID] = conn
	return conn, nil
}

// Disconnect closes and removes a connection.
func (c *Connector) Disconnect(nodeID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	conn, ok := c.connections[nodeID]
	if !ok {
		return fmt.Errorf("no connection to node %q", nodeID)
	}
	conn.State = ConnClosed
	delete(c.connections, nodeID)
	return nil
}

// GetConnection returns the connection for a given node.
func (c *Connector) GetConnection(nodeID string) (*VPSConnection, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	conn, ok := c.connections[nodeID]
	if !ok {
		return nil, fmt.Errorf("no connection to node %q", nodeID)
	}
	return conn, nil
}

// IsConnected checks if a node has an active (ready) connection.
func (c *Connector) IsConnected(nodeID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	conn, ok := c.connections[nodeID]
	return ok && conn.State == ConnReady
}

// ListConnections returns all active connections.
func (c *Connector) ListConnections() []*VPSConnection {
	c.mu.RLock()
	defer c.mu.RUnlock()

	list := make([]*VPSConnection, 0, len(c.connections))
	for _, conn := range c.connections {
		list = append(list, conn)
	}
	return list
}

// ForwardMessage forwards a message to a process on a remote VPS.
// It validates the connection is ready and tracks the forwarding.
func (c *Connector) ForwardMessage(nodeID string, msg ForwardedMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	conn, ok := c.connections[nodeID]
	if !ok {
		return fmt.Errorf("no connection to node %q", nodeID)
	}
	if conn.State != ConnReady {
		return fmt.Errorf("connection to %q is %s, not ready", nodeID, conn.State)
	}

	conn.MessagesSent++
	conn.LastUsed = time.Now()
	conn.Errors = 0 // reset on success

	c.forwarded = append(c.forwarded, msg)
	return nil
}

// RecordError records a connection error. If consecutive errors exceed the
// threshold, the connection is marked unhealthy.
func (c *Connector) RecordError(nodeID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	conn, ok := c.connections[nodeID]
	if !ok {
		return fmt.Errorf("no connection to node %q", nodeID)
	}

	conn.Errors++
	if conn.Errors >= c.maxErrors {
		conn.State = ConnUnhealthy
	}
	return nil
}

// RecordSuccess resets error count and marks connection as ready.
func (c *Connector) RecordSuccess(nodeID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	conn, ok := c.connections[nodeID]
	if !ok {
		return fmt.Errorf("no connection to node %q", nodeID)
	}

	conn.Errors = 0
	if conn.State == ConnUnhealthy {
		conn.State = ConnReady
	}
	conn.LastUsed = time.Now()
	return nil
}

// ForwardedMessages returns the log of forwarded messages (for testing/audit).
func (c *Connector) ForwardedMessages() []ForwardedMessage {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make([]ForwardedMessage, len(c.forwarded))
	copy(result, c.forwarded)
	return result
}

// ConnectionCount returns the number of active connections.
func (c *Connector) ConnectionCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.connections)
}
