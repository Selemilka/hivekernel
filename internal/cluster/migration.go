package cluster

import (
	"fmt"
	"sync"
	"time"

	"github.com/selemilka/hivekernel/internal/process"
)

// MigrationState tracks the lifecycle of a migration.
type MigrationState int

const (
	MigrationPending    MigrationState = iota // queued but not started
	MigrationInProgress                       // shutting down on source / recreating on target
	MigrationCompleted                        // successfully finished
	MigrationFailed                           // failed, processes may need recovery
	MigrationRolledBack                       // failed and rolled back to source
)

func (s MigrationState) String() string {
	switch s {
	case MigrationPending:
		return "pending"
	case MigrationInProgress:
		return "in_progress"
	case MigrationCompleted:
		return "completed"
	case MigrationFailed:
		return "failed"
	case MigrationRolledBack:
		return "rolled_back"
	default:
		return "unknown"
	}
}

// ProcessSnapshot captures the state of a process for migration.
// This is what gets serialized and sent to the target VPS.
type ProcessSnapshot struct {
	PID           process.PID
	PPID          process.PID
	User          string
	Name          string
	Role          process.AgentRole
	CognitiveTier process.CognitiveTier
	Model         string
	State         process.ProcessState
	Limits        process.ResourceLimits
	RuntimeType   string
	RuntimeImage  string
	SystemPrompt  string
	Tools         []string

	// Runtime state
	TokensConsumed      uint64
	ContextUsagePercent float32
	CurrentTaskID       string
}

// SnapshotProcess creates a snapshot from a live process.
func SnapshotProcess(p *process.Process) ProcessSnapshot {
	return ProcessSnapshot{
		PID:                 p.PID,
		PPID:                p.PPID,
		User:                p.User,
		Name:                p.Name,
		Role:                p.Role,
		CognitiveTier:       p.CognitiveTier,
		Model:               p.Model,
		State:               p.State,
		Limits:              p.Limits,
		TokensConsumed:      p.TokensConsumed,
		ContextUsagePercent: p.ContextUsagePercent,
		CurrentTaskID:       p.CurrentTaskID,
	}
}

// Migration represents a single migration operation.
type Migration struct {
	ID         string
	SourceNode string
	TargetNode string
	RootPID    process.PID // the root of the branch being migrated
	Processes  []ProcessSnapshot
	State      MigrationState
	Error      string

	CreatedAt   time.Time
	StartedAt   time.Time
	CompletedAt time.Time
}

// MigrationManager orchestrates process and branch migrations between VPS nodes.
type MigrationManager struct {
	mu         sync.RWMutex
	registry   *process.Registry
	nodes      *NodeRegistry
	migrations map[string]*Migration // id -> migration
	nextID     int
}

// NewMigrationManager creates a new migration manager.
func NewMigrationManager(registry *process.Registry, nodes *NodeRegistry) *MigrationManager {
	return &MigrationManager{
		registry:   registry,
		nodes:      nodes,
		migrations: make(map[string]*Migration),
	}
}

// PrepareMigration creates a migration plan for moving a branch to a target node.
// It snapshots all processes in the branch and validates the target.
func (m *MigrationManager) PrepareMigration(rootPID process.PID, targetNode string) (*Migration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Validate root process exists.
	rootProc, err := m.registry.Get(rootPID)
	if err != nil {
		return nil, fmt.Errorf("root process %d not found: %w", rootPID, err)
	}

	// Cannot migrate the kernel.
	if rootProc.Role == process.RoleKernel {
		return nil, fmt.Errorf("cannot migrate kernel process")
	}

	// Validate target node exists and is online.
	target, err := m.nodes.Get(targetNode)
	if err != nil {
		return nil, fmt.Errorf("target node: %w", err)
	}
	if target.Status != NodeOnline {
		return nil, fmt.Errorf("target node %q is %s, must be online", targetNode, target.Status)
	}

	// Source is current VPS of root process.
	sourceNode := rootProc.VPS
	if sourceNode == targetNode {
		return nil, fmt.Errorf("process already on target node %q", targetNode)
	}

	// Snapshot root + all descendants.
	snapshots := []ProcessSnapshot{SnapshotProcess(rootProc)}
	descendants := m.registry.GetDescendants(rootPID)
	for _, d := range descendants {
		snapshots = append(snapshots, SnapshotProcess(d))
	}

	m.nextID++
	mig := &Migration{
		ID:         fmt.Sprintf("mig-%d", m.nextID),
		SourceNode: sourceNode,
		TargetNode: targetNode,
		RootPID:    rootPID,
		Processes:  snapshots,
		State:      MigrationPending,
		CreatedAt:  time.Now(),
	}
	m.migrations[mig.ID] = mig

	return mig, nil
}

// ExecuteMigration runs a prepared migration.
// Steps: mark draining -> snapshot -> update VPS in registry -> mark complete.
// In production, this would coordinate with the target queen via gRPC.
func (m *MigrationManager) ExecuteMigration(migrationID string) error {
	m.mu.Lock()
	mig, ok := m.migrations[migrationID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("migration %q not found", migrationID)
	}
	if mig.State != MigrationPending {
		m.mu.Unlock()
		return fmt.Errorf("migration %q is %s, expected pending", migrationID, mig.State)
	}

	mig.State = MigrationInProgress
	mig.StartedAt = time.Now()
	m.mu.Unlock()

	// Mark source node as draining (if it was the reason for migration).
	_ = m.nodes.SetStatus(mig.SourceNode, NodeDraining)

	// Update all processes in the branch to the target VPS.
	var migrationErr error
	for _, snap := range mig.Processes {
		err := m.registry.Update(snap.PID, func(p *process.Process) {
			p.VPS = mig.TargetNode
		})
		if err != nil {
			migrationErr = fmt.Errorf("failed to update PID %d: %w", snap.PID, err)
			break
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if migrationErr != nil {
		mig.State = MigrationFailed
		mig.Error = migrationErr.Error()
		// Rollback: restore original VPS.
		for _, snap := range mig.Processes {
			_ = m.registry.Update(snap.PID, func(p *process.Process) {
				p.VPS = mig.SourceNode
			})
		}
		mig.State = MigrationRolledBack
		// Restore source node status.
		_ = m.nodes.SetStatus(mig.SourceNode, NodeOnline)
		return migrationErr
	}

	mig.State = MigrationCompleted
	mig.CompletedAt = time.Now()

	// Update process counts on nodes.
	count := len(mig.Processes)
	if src, err := m.nodes.Get(mig.SourceNode); err == nil {
		src.ProcessCount -= count
		if src.ProcessCount < 0 {
			src.ProcessCount = 0
		}
		// Source may go back to online after losing processes.
		if src.Status == NodeDraining {
			src.Status = NodeOnline
		}
	}
	if tgt, err := m.nodes.Get(mig.TargetNode); err == nil {
		tgt.ProcessCount += count
	}

	return nil
}

// RollbackMigration rolls back a failed or in-progress migration.
func (m *MigrationManager) RollbackMigration(migrationID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	mig, ok := m.migrations[migrationID]
	if !ok {
		return fmt.Errorf("migration %q not found", migrationID)
	}
	if mig.State != MigrationInProgress && mig.State != MigrationFailed {
		return fmt.Errorf("cannot rollback migration in state %s", mig.State)
	}

	// Restore all processes to source VPS.
	for _, snap := range mig.Processes {
		_ = m.registry.Update(snap.PID, func(p *process.Process) {
			p.VPS = mig.SourceNode
		})
	}

	mig.State = MigrationRolledBack
	_ = m.nodes.SetStatus(mig.SourceNode, NodeOnline)
	return nil
}

// GetMigration returns a migration by ID.
func (m *MigrationManager) GetMigration(id string) (*Migration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	mig, ok := m.migrations[id]
	if !ok {
		return nil, fmt.Errorf("migration %q not found", id)
	}
	return mig, nil
}

// ListMigrations returns all migrations.
func (m *MigrationManager) ListMigrations() []*Migration {
	m.mu.RLock()
	defer m.mu.RUnlock()

	list := make([]*Migration, 0, len(m.migrations))
	for _, mig := range m.migrations {
		list = append(list, mig)
	}
	return list
}

// ActiveMigrations returns migrations that are pending or in progress.
func (m *MigrationManager) ActiveMigrations() []*Migration {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var active []*Migration
	for _, mig := range m.migrations {
		if mig.State == MigrationPending || mig.State == MigrationInProgress {
			active = append(active, mig)
		}
	}
	return active
}
