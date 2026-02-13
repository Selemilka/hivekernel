package kernel

import (
	"context"
	"fmt"
	"log"

	pb "github.com/selemilka/hivekernel/api/proto/hivepb"
	"github.com/selemilka/hivekernel/internal/ipc"
	"github.com/selemilka/hivekernel/internal/permissions"
	"github.com/selemilka/hivekernel/internal/process"
	"github.com/selemilka/hivekernel/internal/runtime"
)

// KernelSyscallHandler implements runtime.SyscallHandler by dispatching
// syscalls to the King's subsystems.
type KernelSyscallHandler struct {
	king     *King
	manager  *runtime.Manager
	executor *runtime.Executor
}

// NewKernelSyscallHandler creates a syscall handler backed by the king.
func NewKernelSyscallHandler(king *King, manager *runtime.Manager) *KernelSyscallHandler {
	return &KernelSyscallHandler{king: king, manager: manager}
}

// SetExecutor sets the executor (called after creation to break circular dependency).
func (h *KernelSyscallHandler) SetExecutor(e *runtime.Executor) {
	h.executor = e
}

// HandleSyscall dispatches a single SystemCall and returns its result.
func (h *KernelSyscallHandler) HandleSyscall(
	ctx context.Context,
	callerPID process.PID,
	call *pb.SystemCall,
) *pb.SyscallResult {
	callID := call.CallId
	log.Printf("[syscall] PID %d call_id=%s", callerPID, callID)

	switch c := call.Call.(type) {
	case *pb.SystemCall_Spawn:
		return h.handleSpawn(callID, callerPID, c.Spawn)
	case *pb.SystemCall_Kill:
		return h.handleKill(callID, callerPID, c.Kill)
	case *pb.SystemCall_Send:
		return h.handleSend(callID, callerPID, c.Send)
	case *pb.SystemCall_Store:
		return h.handleStore(callID, callerPID, c.Store)
	case *pb.SystemCall_GetArtifact:
		return h.handleGetArtifact(callID, callerPID, c.GetArtifact)
	case *pb.SystemCall_Escalate:
		return h.handleEscalate(callID, callerPID, c.Escalate)
	case *pb.SystemCall_Log:
		return h.handleLog(callID, callerPID, c.Log)
	case *pb.SystemCall_ExecuteOn:
		return h.handleExecuteOn(ctx, callID, callerPID, c.ExecuteOn)
	default:
		log.Printf("[syscall] unknown syscall type for call_id=%s", callID)
		return &pb.SyscallResult{CallId: callID}
	}
}

func (h *KernelSyscallHandler) handleSpawn(callID string, callerPID process.PID, req *pb.SpawnRequest) *pb.SyscallResult {
	proc, err := h.king.SpawnChild(process.SpawnRequest{
		ParentPID:     callerPID,
		Name:          req.Name,
		Role:          protoRoleToInternal(req.Role),
		CognitiveTier: protoCogToInternal(req.CognitiveTier),
		Model:         req.Model,
		SystemPrompt:  req.SystemPrompt,
		Tools:         req.Tools,
		Limits:        protoLimitsToInternal(req.Limits),
		InitialTask:   req.InitialTask,
		RuntimeType:   req.RuntimeType.String(),
		RuntimeImage:  req.RuntimeImage,
	})
	if err != nil {
		return &pb.SyscallResult{
			CallId: callID,
			Result: &pb.SyscallResult_Spawn{
				Spawn: &pb.SpawnResponse{Success: false, Error: err.Error()},
			},
		}
	}
	log.Printf("[syscall] spawn: PID %d spawned %s (PID %d)", callerPID, proc.Name, proc.PID)
	return &pb.SyscallResult{
		CallId: callID,
		Result: &pb.SyscallResult_Spawn{
			Spawn: &pb.SpawnResponse{Success: true, ChildPid: proc.PID},
		},
	}
}

func (h *KernelSyscallHandler) handleKill(callID string, callerPID process.PID, req *pb.KillRequest) *pb.SyscallResult {
	target, err := h.king.Registry().Get(req.TargetPid)
	if err != nil {
		return &pb.SyscallResult{
			CallId: callID,
			Result: &pb.SyscallResult_Kill{
				Kill: &pb.KillResponse{Success: false, Error: "process not found"},
			},
		}
	}
	if target.PPID != callerPID {
		return &pb.SyscallResult{
			CallId: callID,
			Result: &pb.SyscallResult_Kill{
				Kill: &pb.KillResponse{Success: false, Error: "can only kill own children"},
			},
		}
	}

	var killed []uint64
	if req.Recursive {
		descendants := h.king.Registry().GetDescendants(req.TargetPid)
		for _, d := range descendants {
			if h.manager != nil {
				_ = h.manager.StopRuntime(d.PID)
			}
			_ = h.king.Registry().SetState(d.PID, process.StateDead)
			killed = append(killed, d.PID)
		}
	}
	if h.manager != nil {
		_ = h.manager.StopRuntime(req.TargetPid)
	}
	_ = h.king.Registry().SetState(req.TargetPid, process.StateDead)
	killed = append(killed, req.TargetPid)

	log.Printf("[syscall] kill: PID %d killed %v", callerPID, killed)
	return &pb.SyscallResult{
		CallId: callID,
		Result: &pb.SyscallResult_Kill{
			Kill: &pb.KillResponse{Success: true, KilledPids: killed},
		},
	}
}

func (h *KernelSyscallHandler) handleSend(callID string, callerPID process.PID, req *pb.SendMessageRequest) *pb.SyscallResult {
	// ACL check.
	if err := h.king.ACL().Check(callerPID, permissions.ActionSendMessage); err != nil {
		return &pb.SyscallResult{
			CallId: callID,
			Result: &pb.SyscallResult_Send{
				Send: &pb.SendMessageResponse{Delivered: false, Error: err.Error()},
			},
		}
	}
	// Rate limit check.
	if !h.king.RateLimiter().Allow(callerPID) {
		return &pb.SyscallResult{
			CallId: callID,
			Result: &pb.SyscallResult_Send{
				Send: &pb.SendMessageResponse{Delivered: false, Error: "rate limit exceeded"},
			},
		}
	}

	sender, err := h.king.Registry().Get(callerPID)
	if err != nil {
		return &pb.SyscallResult{
			CallId: callID,
			Result: &pb.SyscallResult_Send{
				Send: &pb.SendMessageResponse{Delivered: false, Error: fmt.Sprintf("sender %d not found", callerPID)},
			},
		}
	}

	msg := &ipc.Message{
		FromPID:     callerPID,
		FromName:    sender.Name,
		ToPID:       req.ToPid,
		ToQueue:     req.ToQueue,
		Type:        req.Type,
		Priority:    int(req.Priority),
		Payload:     req.Payload,
		RequiresAck: req.RequiresAck,
	}

	if err := h.king.Broker().Route(msg); err != nil {
		return &pb.SyscallResult{
			CallId: callID,
			Result: &pb.SyscallResult_Send{
				Send: &pb.SendMessageResponse{Delivered: false, Error: err.Error()},
			},
		}
	}

	log.Printf("[syscall] send: PID %d -> PID %d, type=%s", callerPID, req.ToPid, req.Type)
	return &pb.SyscallResult{
		CallId: callID,
		Result: &pb.SyscallResult_Send{
			Send: &pb.SendMessageResponse{Delivered: true, MessageId: msg.ID},
		},
	}
}

func (h *KernelSyscallHandler) handleStore(callID string, callerPID process.PID, req *pb.StoreArtifactRequest) *pb.SyscallResult {
	// ACL check.
	if err := h.king.ACL().Check(callerPID, permissions.ActionWriteArtifact); err != nil {
		return &pb.SyscallResult{
			CallId: callID,
			Result: &pb.SyscallResult_Store{
				Store: &pb.StoreArtifactResponse{Success: false, Error: err.Error()},
			},
		}
	}

	vis := ipc.Visibility(req.Visibility)
	id, err := h.king.SharedMemory().Store(callerPID, req.Key, req.Content, req.ContentType, vis)
	if err != nil {
		return &pb.SyscallResult{
			CallId: callID,
			Result: &pb.SyscallResult_Store{
				Store: &pb.StoreArtifactResponse{Success: false, Error: err.Error()},
			},
		}
	}

	log.Printf("[syscall] store: PID %d stored %q (id=%s)", callerPID, req.Key, id)
	return &pb.SyscallResult{
		CallId: callID,
		Result: &pb.SyscallResult_Store{
			Store: &pb.StoreArtifactResponse{Success: true, ArtifactId: id},
		},
	}
}

func (h *KernelSyscallHandler) handleGetArtifact(callID string, callerPID process.PID, req *pb.GetArtifactRequest) *pb.SyscallResult {
	var art *ipc.Artifact
	var err error
	if req.Key != "" {
		art, err = h.king.SharedMemory().Get(callerPID, req.Key)
	} else {
		art, err = h.king.SharedMemory().GetByID(callerPID, req.ArtifactId)
	}
	if err != nil {
		return &pb.SyscallResult{
			CallId: callID,
			Result: &pb.SyscallResult_GetArtifact{
				GetArtifact: &pb.GetArtifactResponse{Found: false, Error: err.Error()},
			},
		}
	}
	return &pb.SyscallResult{
		CallId: callID,
		Result: &pb.SyscallResult_GetArtifact{
			GetArtifact: &pb.GetArtifactResponse{
				Found:       true,
				Content:     art.Content,
				ContentType: art.ContentType,
				StoredByPid: art.StoredByPID,
				StoredAt:    uint64(art.StoredAt.Unix()),
			},
		},
	}
}

func (h *KernelSyscallHandler) handleEscalate(callID string, callerPID process.PID, req *pb.EscalateRequest) *pb.SyscallResult {
	log.Printf("[syscall] escalate from PID %d: severity=%s issue=%s", callerPID, req.Severity, req.Issue)
	h.king.Inbox().Push(&ipc.Message{
		FromPID:  callerPID,
		ToPID:    h.king.PID(),
		Type:     "escalation",
		Priority: ipc.PriorityHigh,
		Payload:  []byte(req.Issue),
	})
	return &pb.SyscallResult{
		CallId: callID,
		Result: &pb.SyscallResult_Escalate{
			Escalate: &pb.EscalateResponse{Received: true, HandledByPid: h.king.PID()},
		},
	}
}

func (h *KernelSyscallHandler) handleLog(callID string, callerPID process.PID, req *pb.LogRequest) *pb.SyscallResult {
	log.Printf("[agent:%d] [%s] %s", callerPID, req.Level, req.Message)

	// If tokens_consumed metric, record in accounting (same as ReportMetric).
	if req.Level == pb.LogLevel_LOG_INFO && req.Message != "" {
		// Logging via syscall is just logging. Token accounting is via ReportMetric.
	}

	return &pb.SyscallResult{
		CallId: callID,
		Result: &pb.SyscallResult_Log{
			Log: &pb.LogResponse{},
		},
	}
}

func (h *KernelSyscallHandler) handleExecuteOn(ctx context.Context, callID string, callerPID process.PID, req *pb.ExecuteOnRequest) *pb.SyscallResult {
	errResult := func(msg string) *pb.SyscallResult {
		return &pb.SyscallResult{
			CallId: callID,
			Result: &pb.SyscallResult_ExecuteOn{
				ExecuteOn: &pb.ExecuteOnResponse{Success: false, Error: msg},
			},
		}
	}

	// Validate target_pid is a child of callerPID.
	target, err := h.king.Registry().Get(req.TargetPid)
	if err != nil {
		return errResult("target process not found")
	}
	if target.PPID != callerPID {
		return errResult("can only execute_on own children")
	}

	// Get target's runtime address.
	if h.manager == nil {
		return errResult("runtime manager not available")
	}
	rt := h.manager.GetRuntime(req.TargetPid)
	if rt == nil {
		return errResult(fmt.Sprintf("no runtime for PID %d", req.TargetPid))
	}

	if h.executor == nil {
		return errResult("executor not available")
	}

	// Execute the task on the target agent.
	log.Printf("[syscall] execute_on: PID %d -> PID %d, task=%s", callerPID, req.TargetPid, req.Task.GetDescription())
	taskResult, err := h.executor.ExecuteTask(ctx, rt.Addr, req.TargetPid, req.Task)
	if err != nil {
		return errResult(fmt.Sprintf("execute_on failed: %v", err))
	}

	return &pb.SyscallResult{
		CallId: callID,
		Result: &pb.SyscallResult_ExecuteOn{
			ExecuteOn: &pb.ExecuteOnResponse{
				Success: true,
				Result:  taskResult,
			},
		},
	}
}

