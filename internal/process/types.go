package process

import (
	"time"
)

// PID is a unique process identifier.
type PID = uint64

// AgentRole determines lifecycle and behavior.
type AgentRole int

const (
	RoleKernel    AgentRole = iota // permanent, single instance
	RoleDaemon                     // permanent, auto-restart
	RoleAgent                      // permanent, long-lived
	RoleArchitect                  // ephemeral / sleep
	RoleLead                       // long-lived, coordinates workers
	RoleWorker                     // long-lived, executor
	RoleTask                       // short-lived, atomic
)

func (r AgentRole) String() string {
	switch r {
	case RoleKernel:
		return "kernel"
	case RoleDaemon:
		return "daemon"
	case RoleAgent:
		return "agent"
	case RoleArchitect:
		return "architect"
	case RoleLead:
		return "lead"
	case RoleWorker:
		return "worker"
	case RoleTask:
		return "task"
	default:
		return "unknown"
	}
}

// CognitiveTier determines model selection.
type CognitiveTier int

const (
	CogStrategic   CognitiveTier = iota // opus
	CogTactical                          // sonnet
	CogOperational                       // mini/haiku
)

func (c CognitiveTier) String() string {
	switch c {
	case CogStrategic:
		return "strategic"
	case CogTactical:
		return "tactical"
	case CogOperational:
		return "operational"
	default:
		return "unknown"
	}
}

// ProcessState represents the current state of a process.
type ProcessState int

const (
	StateIdle     ProcessState = iota
	StateRunning
	StateBlocked
	StateSleeping
	StateZombie
	StateDead
)

func (s ProcessState) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateRunning:
		return "running"
	case StateBlocked:
		return "blocked"
	case StateSleeping:
		return "sleeping"
	case StateZombie:
		return "zombie"
	case StateDead:
		return "dead"
	default:
		return "unknown"
	}
}

// ResourceLimits defines per-process resource constraints.
type ResourceLimits struct {
	MaxTokensPerHour   uint64
	MaxContextTokens   uint64
	MaxChildren        uint32
	MaxConcurrentTasks uint32
	TimeoutSeconds     uint32
	MaxTokensTotal     uint64
}

// Process represents an entry in the process table.
type Process struct {
	PID           PID
	PPID          PID
	User          string
	Name          string
	Role          AgentRole
	CognitiveTier CognitiveTier
	Model         string
	VPS           string
	State         ProcessState
	Limits        ResourceLimits

	// Runtime stats
	TokensConsumed      uint64
	ContextUsagePercent float32
	CurrentTaskID       string

	// Timestamps
	StartedAt time.Time
	UpdatedAt time.Time

	// gRPC connection address (unix socket path or tcp addr)
	RuntimeAddr string
}

// DefaultModelForTier returns the default model string for a cognitive tier.
func DefaultModelForTier(tier CognitiveTier) string {
	switch tier {
	case CogStrategic:
		return "opus"
	case CogTactical:
		return "sonnet"
	case CogOperational:
		return "mini"
	default:
		return "mini"
	}
}
