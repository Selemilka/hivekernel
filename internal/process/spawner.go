package process

import (
	"fmt"
)

// SpawnRequest contains everything needed to spawn a new child process.
type SpawnRequest struct {
	ParentPID     PID
	Name          string
	Role          AgentRole
	CognitiveTier CognitiveTier
	Model         string // empty = auto by cognitive tier
	User          string // empty = inherit from parent
	VPS           string // empty = same as parent
	SystemPrompt  string
	Metadata      map[string]string
	Tools         []string
	Limits        ResourceLimits
	InitialTask   string
	RuntimeType   string // "python", "claw", "custom"
	RuntimeImage  string
}

// SpawnResult is returned after a successful spawn.
type SpawnResult struct {
	PID         PID
	RuntimeAddr string
}

// Spawner validates and creates new processes.
type Spawner struct {
	registry *Registry
}

// NewSpawner creates a spawner backed by the given registry.
func NewSpawner(registry *Registry) *Spawner {
	return &Spawner{registry: registry}
}

// Spawn validates the request and registers a new process.
// It does NOT start the runtime — that's the runtime manager's job.
func (s *Spawner) Spawn(req SpawnRequest) (*Process, error) {
	// Validate parent exists (except for PID 1 — kernel bootstraps itself).
	var parent *Process
	if req.ParentPID != 0 {
		var err error
		parent, err = s.registry.Get(req.ParentPID)
		if err != nil {
			return nil, fmt.Errorf("spawn: parent %d not found: %w", req.ParentPID, err)
		}
	}

	if err := s.validate(req, parent); err != nil {
		return nil, fmt.Errorf("spawn: validation failed: %w", err)
	}

	// Resolve defaults.
	model := req.Model
	if model == "" {
		model = DefaultModelForTier(req.CognitiveTier)
	}
	user := req.User
	if user == "" && parent != nil {
		user = parent.User
	}
	vps := req.VPS
	if vps == "" && parent != nil {
		vps = parent.VPS
	}

	proc := &Process{
		PPID:          req.ParentPID,
		User:          user,
		Name:          req.Name,
		Role:          req.Role,
		CognitiveTier: req.CognitiveTier,
		Model:         model,
		VPS:           vps,
		State:         StateIdle,
		Limits:        req.Limits,
		SystemPrompt:  req.SystemPrompt,
		Metadata:      req.Metadata,
		RuntimeType:   req.RuntimeType,
		RuntimeImage:  req.RuntimeImage,
	}

	pid, err := s.registry.Register(proc)
	if err != nil {
		return nil, fmt.Errorf("spawn: register failed: %w", err)
	}
	_ = pid // already set on proc by Register

	return proc, nil
}

// validate checks all spawn invariants from the spec.
func (s *Spawner) validate(req SpawnRequest, parent *Process) error {
	// Rule: dead/zombie process cannot spawn children.
	if parent != nil && (parent.State == StateDead || parent.State == StateZombie) {
		return fmt.Errorf("parent %d is %s and cannot spawn children", parent.PID, parent.State)
	}

	// Rule: cognitive tier of child <= parent.
	if parent != nil {
		if req.CognitiveTier < parent.CognitiveTier {
			return fmt.Errorf(
				"child cognitive tier %s exceeds parent %s (child must be <= parent)",
				req.CognitiveTier, parent.CognitiveTier,
			)
		}
	}

	// Rule: max_children not exceeded.
	if parent != nil && parent.Limits.MaxChildren > 0 {
		count := s.registry.CountChildren(parent.PID)
		if uint32(count) >= parent.Limits.MaxChildren {
			return fmt.Errorf(
				"parent %d has %d children, max is %d",
				parent.PID, count, parent.Limits.MaxChildren,
			)
		}
	}

	// Rule: role compatible with cognitive tier.
	// From spec: "task cannot be strategic".
	if req.Role == RoleTask && req.CognitiveTier == CogStrategic {
		return fmt.Errorf("task role cannot have strategic cognitive tier")
	}

	// Rule: name is required.
	if req.Name == "" {
		return fmt.Errorf("process name is required")
	}

	return nil
}

// SpawnKernel bootstraps PID 1 (the king process). No parent validation.
func (s *Spawner) SpawnKernel(name, user, vps string) (*Process, error) {
	proc := &Process{
		PPID:          0, // no parent
		User:          user,
		Name:          name,
		Role:          RoleKernel,
		CognitiveTier: CogStrategic,
		Model:         "opus",
		VPS:           vps,
		State:         StateRunning,
		Limits: ResourceLimits{
			MaxChildren: 100,
		},
	}

	pid, err := s.registry.Register(proc)
	if err != nil {
		return nil, fmt.Errorf("spawn kernel: %w", err)
	}
	_ = pid
	return proc, nil
}
