package process

import (
	"context"
	"sync"
	"time"

	"github.com/selemilka/hivekernel/internal/hklog"
)

// RestartPolicy defines how a process should be handled when it crashes.
type RestartPolicy int

const (
	RestartAlways  RestartPolicy = iota // Always restart (daemons)
	RestartNotify                        // Notify parent, let them decide (agents, workers)
	RestartNever                         // Never restart (tasks)
)

// RestartPolicyForRole returns the default restart policy for a given role.
func RestartPolicyForRole(role AgentRole) RestartPolicy {
	switch role {
	case RoleKernel:
		return RestartAlways // should never crash, but if it does...
	case RoleDaemon:
		return RestartAlways
	case RoleAgent:
		return RestartNotify
	case RoleLead:
		return RestartNotify
	case RoleWorker:
		return RestartNotify
	case RoleArchitect:
		return RestartNever
	case RoleTask:
		return RestartNever
	default:
		return RestartNever
	}
}

// SupervisorEvent represents something the supervisor detected.
type SupervisorEvent struct {
	Type      string // "crashed", "zombie", "timeout", "restarted"
	PID       PID
	ParentPID PID
	Name      string
	Details   string
	Time      time.Time
}

// SupervisorConfig holds tuning parameters for the supervisor.
type SupervisorConfig struct {
	ZombieScanInterval  time.Duration // How often to scan for zombies
	ZombieTimeout       time.Duration // How long before a zombie is reaped
	MaxRestartAttempts  int           // Max restarts before giving up
	RestartBackoff      time.Duration // Delay between restart attempts
}

// DefaultSupervisorConfig returns sensible defaults.
func DefaultSupervisorConfig() SupervisorConfig {
	return SupervisorConfig{
		ZombieScanInterval: 15 * time.Second,
		ZombieTimeout:      60 * time.Second,
		MaxRestartAttempts: 3,
		RestartBackoff:     5 * time.Second,
	}
}

// Supervisor monitors the process tree and enforces lifecycle rules.
type Supervisor struct {
	registry *Registry
	signals  *SignalRouter
	tree     *TreeOps
	config   SupervisorConfig

	// Track restart attempts per PID.
	mu             sync.Mutex
	restartCounts  map[PID]int
	lastRestart    map[PID]time.Time

	// Event channel for external consumers (queen, maid, etc.)
	events chan SupervisorEvent

	// Callback for restart: the actual logic to restart is provided externally
	// because it depends on the runtime manager.
	onRestart func(proc *Process) error
}

// NewSupervisor creates a new supervisor.
func NewSupervisor(
	registry *Registry,
	signals *SignalRouter,
	tree *TreeOps,
	config SupervisorConfig,
) *Supervisor {
	return &Supervisor{
		registry:      registry,
		signals:       signals,
		tree:          tree,
		config:        config,
		restartCounts: make(map[PID]int),
		lastRestart:   make(map[PID]time.Time),
		events:        make(chan SupervisorEvent, 100),
	}
}

// Events returns a channel of supervisor events for external consumers.
func (s *Supervisor) Events() <-chan SupervisorEvent {
	return s.events
}

// OnRestart sets the callback invoked when a process needs restarting.
func (s *Supervisor) OnRestart(fn func(proc *Process) error) {
	s.onRestart = fn
}

// HandleChildExit is called when SIGCHLD is received — a child has exited.
// It decides whether to restart, notify parent, or clean up.
func (s *Supervisor) HandleChildExit(exitedPID PID, exitCode int) {
	proc, err := s.registry.Get(exitedPID)
	if err != nil {
		return
	}

	policy := RestartPolicyForRole(proc.Role)

	hklog.For("supervisor").Info("child exited", "pid", exitedPID, "name", proc.Name, "exit_code", exitCode, "policy", policy)

	switch policy {
	case RestartAlways:
		s.attemptRestart(proc, exitCode)

	case RestartNotify:
		// Mark zombie and notify parent, let them decide.
		_ = s.registry.SetState(exitedPID, StateZombie)
		s.signals.NotifyParent(exitedPID, exitCode, "")
		s.emitEvent("crashed", proc, "notified parent")

	case RestartNever:
		// Mark zombie and notify parent.
		_ = s.registry.SetState(exitedPID, StateZombie)
		s.signals.NotifyParent(exitedPID, exitCode, "")
		s.emitEvent("crashed", proc, "no restart (task)")
	}
}

// attemptRestart tries to restart a crashed process with backoff.
func (s *Supervisor) attemptRestart(proc *Process, exitCode int) {
	s.mu.Lock()
	count := s.restartCounts[proc.PID]
	lastTime := s.lastRestart[proc.PID]
	s.mu.Unlock()

	// Reset counter if it's been a while since last restart.
	if time.Since(lastTime) > 5*time.Minute {
		count = 0
	}

	if count >= s.config.MaxRestartAttempts {
		hklog.For("supervisor").Warn("max restart attempts exceeded", "pid", proc.PID, "name", proc.Name, "max_attempts", s.config.MaxRestartAttempts)
		_ = s.registry.SetState(proc.PID, StateDead)
		s.signals.NotifyParent(proc.PID, exitCode, "max restarts exceeded")
		s.emitEvent("crashed", proc, "max restarts exceeded, notified parent")
		return
	}

	// Backoff before restart.
	backoff := s.config.RestartBackoff * time.Duration(count+1)
	hklog.For("supervisor").Info("restarting process", "pid", proc.PID, "name", proc.Name, "backoff", backoff, "attempt", count+1, "max_attempts", s.config.MaxRestartAttempts)

	s.mu.Lock()
	s.restartCounts[proc.PID] = count + 1
	s.lastRestart[proc.PID] = time.Now()
	s.mu.Unlock()

	go func() {
		time.Sleep(backoff)

		if s.onRestart != nil {
			if err := s.onRestart(proc); err != nil {
				hklog.For("supervisor").Error("restart failed", "pid", proc.PID, "error", err)
				s.emitEvent("crashed", proc, "restart failed: "+err.Error())
				return
			}
		}

		_ = s.registry.SetState(proc.PID, StateRunning)
		s.emitEvent("restarted", proc, "")
		hklog.For("supervisor").Info("restarted successfully", "pid", proc.PID, "name", proc.Name)
	}()
}

// Run starts the supervisor's background loops. Blocks until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) {
	zombieTicker := time.NewTicker(s.config.ZombieScanInterval)
	defer zombieTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-zombieTicker.C:
			s.reapZombies()
		}
	}
}

// reapZombies finds and cleans up zombie processes.
// A zombie is a process that has exited but whose parent hasn't collected the result.
func (s *Supervisor) reapZombies() {
	now := time.Now()
	for _, p := range s.registry.List() {
		if p.State != StateZombie {
			continue
		}
		if now.Sub(p.UpdatedAt) > s.config.ZombieTimeout {
			hklog.For("supervisor").Info("reaping zombie", "pid", p.PID, "name", p.Name, "zombie_duration", now.Sub(p.UpdatedAt).Round(time.Second))

			// Notify parent one more time.
			s.signals.NotifyParent(p.PID, -1, "zombie reaped")

			// Kill any orphaned children.
			children := s.registry.GetChildren(p.PID)
			for _, child := range children {
				if child.State != StateDead {
					s.tree.Reparent(child.PID, p.PPID)
				}
			}

			// Remove from registry.
			_ = s.registry.Remove(p.PID)
			s.emitEvent("zombie", p, "reaped")
		}
	}
}

// ResetRestartCount clears the restart counter for a process (e.g. after stable uptime).
func (s *Supervisor) ResetRestartCount(pid PID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.restartCounts, pid)
	delete(s.lastRestart, pid)
}

func (s *Supervisor) emitEvent(typ string, proc *Process, details string) {
	evt := SupervisorEvent{
		Type:      typ,
		PID:       proc.PID,
		ParentPID: proc.PPID,
		Name:      proc.Name,
		Details:   details,
		Time:      time.Now(),
	}
	select {
	case s.events <- evt:
	default:
		// Channel full, drop event.
		hklog.For("supervisor").Warn("event channel full, dropping event", "event_type", evt.Type, "pid", evt.PID)
	}
}
