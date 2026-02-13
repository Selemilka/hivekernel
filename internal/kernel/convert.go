package kernel

import (
	"github.com/selemilka/hivekernel/internal/process"

	pb "github.com/selemilka/hivekernel/api/proto/hivepb"
)

// --- Proto → Internal ---

func protoRoleToInternal(r pb.AgentRole) process.AgentRole {
	switch r {
	case pb.AgentRole_ROLE_KERNEL:
		return process.RoleKernel
	case pb.AgentRole_ROLE_DAEMON:
		return process.RoleDaemon
	case pb.AgentRole_ROLE_AGENT:
		return process.RoleAgent
	case pb.AgentRole_ROLE_ARCHITECT:
		return process.RoleArchitect
	case pb.AgentRole_ROLE_LEAD:
		return process.RoleLead
	case pb.AgentRole_ROLE_WORKER:
		return process.RoleWorker
	case pb.AgentRole_ROLE_TASK:
		return process.RoleTask
	default:
		return process.RoleTask
	}
}

func protoCogToInternal(c pb.CognitiveTier) process.CognitiveTier {
	switch c {
	case pb.CognitiveTier_COG_STRATEGIC:
		return process.CogStrategic
	case pb.CognitiveTier_COG_TACTICAL:
		return process.CogTactical
	case pb.CognitiveTier_COG_OPERATIONAL:
		return process.CogOperational
	default:
		return process.CogOperational
	}
}

func protoLimitsToInternal(l *pb.ResourceLimits) process.ResourceLimits {
	if l == nil {
		return process.ResourceLimits{}
	}
	return process.ResourceLimits{
		MaxTokensPerHour:   l.MaxTokensPerHour,
		MaxContextTokens:   l.MaxContextTokens,
		MaxChildren:        l.MaxChildren,
		MaxConcurrentTasks: l.MaxConcurrentTasks,
		TimeoutSeconds:     l.TimeoutSeconds,
		MaxTokensTotal:     l.MaxTokensTotal,
	}
}

// --- Internal → Proto ---

func internalRoleToProto(r process.AgentRole) pb.AgentRole {
	switch r {
	case process.RoleKernel:
		return pb.AgentRole_ROLE_KERNEL
	case process.RoleDaemon:
		return pb.AgentRole_ROLE_DAEMON
	case process.RoleAgent:
		return pb.AgentRole_ROLE_AGENT
	case process.RoleArchitect:
		return pb.AgentRole_ROLE_ARCHITECT
	case process.RoleLead:
		return pb.AgentRole_ROLE_LEAD
	case process.RoleWorker:
		return pb.AgentRole_ROLE_WORKER
	case process.RoleTask:
		return pb.AgentRole_ROLE_TASK
	default:
		return pb.AgentRole_ROLE_TASK
	}
}

func internalCogToProto(c process.CognitiveTier) pb.CognitiveTier {
	switch c {
	case process.CogStrategic:
		return pb.CognitiveTier_COG_STRATEGIC
	case process.CogTactical:
		return pb.CognitiveTier_COG_TACTICAL
	case process.CogOperational:
		return pb.CognitiveTier_COG_OPERATIONAL
	default:
		return pb.CognitiveTier_COG_OPERATIONAL
	}
}

func internalStateToProto(s process.ProcessState) pb.AgentState {
	switch s {
	case process.StateIdle:
		return pb.AgentState_STATE_IDLE
	case process.StateRunning:
		return pb.AgentState_STATE_RUNNING
	case process.StateBlocked:
		return pb.AgentState_STATE_BLOCKED
	case process.StateSleeping:
		return pb.AgentState_STATE_SLEEPING
	case process.StateDead:
		return pb.AgentState_STATE_DEAD
	case process.StateZombie:
		return pb.AgentState_STATE_ZOMBIE
	default:
		return pb.AgentState_STATE_IDLE
	}
}

func processToProto(p *process.Process) *pb.ProcessInfo {
	return &pb.ProcessInfo{
		Pid:                 p.PID,
		Ppid:                p.PPID,
		User:                p.User,
		Name:                p.Name,
		Role:                internalRoleToProto(p.Role),
		CognitiveTier:       internalCogToProto(p.CognitiveTier),
		Model:               p.Model,
		State:               internalStateToProto(p.State),
		Vps:                 p.VPS,
		TokensConsumed:      p.TokensConsumed,
		ContextUsagePercent: p.ContextUsagePercent,
		ChildCount:          uint32(0), // filled by caller if needed
		StartedAt:           uint64(p.StartedAt.Unix()),
		CurrentTaskId:       p.CurrentTaskID,
		RuntimeAddr:         p.RuntimeAddr,
	}
}
