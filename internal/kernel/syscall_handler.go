package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

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
	case *pb.SystemCall_WaitChild:
		return h.handleWaitChild(ctx, callID, callerPID, c.WaitChild)
	case *pb.SystemCall_ListSiblings:
		return h.handleListSiblings(callID, callerPID)
	default:
		log.Printf("[syscall] unknown syscall type for call_id=%s", callID)
		return &pb.SyscallResult{CallId: callID}
	}
}

// extractTraceFromPayload extracts trace_id and trace_span from a JSON payload.
func extractTraceFromPayload(payload []byte) (traceID, traceSpan string) {
	var data map[string]interface{}
	if json.Unmarshal(payload, &data) != nil {
		return "", ""
	}
	traceID, _ = data["trace_id"].(string)
	traceSpan, _ = data["trace_span"].(string)
	return
}

// extractTraceFromParams extracts trace_id and trace_span from a string map.
func extractTraceFromParams(params map[string]string) (traceID, traceSpan string) {
	return params["trace_id"], params["trace_span"]
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
		RuntimeType:   ParseRuntimeType(req.RuntimeType.String()),
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
			_ = h.king.Registry().SetState(d.PID, process.StateZombie)
			h.king.Signals().NotifyParent(d.PID, -1, "killed")
			killed = append(killed, d.PID)
		}
	}
	if h.manager != nil {
		_ = h.manager.StopRuntime(req.TargetPid)
	}
	_ = h.king.Registry().SetState(req.TargetPid, process.StateZombie)
	h.king.Signals().NotifyParent(req.TargetPid, -1, "killed")
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
		ReplyTo:     req.ReplyTo,
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

	// Extract trace context from payload and propagate to receiver.
	traceID, traceSpan := extractTraceFromPayload(req.Payload)
	if traceID != "" {
		h.king.SetTrace(req.ToPid, traceID, traceSpan)
	}

	// Emit message_sent event for dashboard visualization.
	if el := h.king.EventLog(); el != nil {
		toName := ""
		if receiver, err := h.king.Registry().Get(req.ToPid); err == nil {
			toName = receiver.Name
		}
		el.Emit(process.ProcessEvent{
			Type:           process.EventMessageSent,
			PID:            callerPID,
			PPID:           req.ToPid,
			Name:           sender.Name,
			Role:           toName,
			Message:        req.Type,
			ReplyTo:        req.ReplyTo,
			PayloadPreview: truncatePayload(string(req.Payload), 2000),
			TraceID:        traceID,
			TraceSpan:      traceSpan,
		})
	}

	return &pb.SyscallResult{
		CallId: callID,
		Result: &pb.SyscallResult_Send{
			Send: &pb.SendMessageResponse{Delivered: true, MessageId: msg.ID},
		},
	}
}

// truncatePayload returns s truncated to maxLen runes, appending "..." if truncated.
func truncatePayload(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
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
	levelStr := "info"
	switch req.Level {
	case pb.LogLevel_LOG_DEBUG:
		levelStr = "debug"
	case pb.LogLevel_LOG_WARN:
		levelStr = "warn"
	case pb.LogLevel_LOG_ERROR:
		levelStr = "error"
	}
	log.Printf("[agent:%d] [%s] %s", callerPID, levelStr, req.Message)

	// Emit log event so dashboard can display it.
	if el := h.king.EventLog(); el != nil {
		// Look up process name for richer log context.
		name := ""
		if p, err := h.king.Registry().Get(callerPID); err == nil {
			name = p.Name
		}
		el.Emit(process.ProcessEvent{
			Type:    process.EventLogged,
			PID:     callerPID,
			Name:    name,
			Level:   levelStr,
			Message: req.Message,
		})
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

	// Extract trace context from task params and propagate to target.
	// If not in params, inherit from caller's trace context.
	traceSet := false
	if req.Task != nil && req.Task.Params != nil {
		if tID, tSpan := extractTraceFromParams(req.Task.Params); tID != "" {
			h.king.SetTrace(req.TargetPid, tID, tSpan)
			traceSet = true
		}
	}
	if !traceSet {
		if tID, tSpan := h.king.GetTrace(callerPID); tID != "" {
			h.king.SetTrace(req.TargetPid, tID, tSpan)
		}
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

func (h *KernelSyscallHandler) handleListSiblings(callID string, callerPID process.PID) *pb.SyscallResult {
	caller, err := h.king.Registry().Get(callerPID)
	if err != nil {
		return &pb.SyscallResult{
			CallId: callID,
			Result: &pb.SyscallResult_ListSiblings{
				ListSiblings: &pb.ListSiblingsResponse{},
			},
		}
	}

	// Get parent's children (caller's siblings).
	children := h.king.Registry().GetChildren(caller.PPID)
	var siblings []*pb.SiblingInfo
	for _, child := range children {
		if child.PID == callerPID {
			continue // exclude self
		}
		siblings = append(siblings, &pb.SiblingInfo{
			Pid:   child.PID,
			Name:  child.Name,
			Role:  child.Role.String(),
			State: child.State.String(),
		})
	}

	log.Printf("[syscall] list_siblings: PID %d found %d siblings", callerPID, len(siblings))
	return &pb.SyscallResult{
		CallId: callID,
		Result: &pb.SyscallResult_ListSiblings{
			ListSiblings: &pb.ListSiblingsResponse{Siblings: siblings},
		},
	}
}

func (h *KernelSyscallHandler) handleWaitChild(ctx context.Context, callID string, callerPID process.PID, req *pb.WaitChildRequest) *pb.SyscallResult {
	errResult := func(msg string) *pb.SyscallResult {
		return &pb.SyscallResult{
			CallId: callID,
			Result: &pb.SyscallResult_WaitChild{
				WaitChild: &pb.WaitChildResponse{Success: false, Error: msg},
			},
		}
	}

	okResult := func(pid process.PID, exitCode int32, output string) *pb.SyscallResult {
		return &pb.SyscallResult{
			CallId: callID,
			Result: &pb.SyscallResult_WaitChild{
				WaitChild: &pb.WaitChildResponse{
					Success:  true,
					Pid:      pid,
					ExitCode: exitCode,
					Output:   output,
				},
			},
		}
	}

	// Validate target is a child of caller.
	target, err := h.king.Registry().Get(req.TargetPid)
	if err != nil {
		return errResult("process not found")
	}
	if target.PPID != callerPID {
		return errResult("can only wait for own children")
	}

	// If already zombie/dead, collect result immediately.
	if target.State == process.StateZombie || target.State == process.StateDead {
		result, _ := h.king.Lifecycle().WaitResult(target.PID)
		if result != nil {
			return okResult(target.PID, int32(result.ExitCode), string(result.Output))
		}
		return okResult(target.PID, -1, "")
	}

	// Process is still alive — poll until it dies or timeout.
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	deadline := time.After(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return errResult("context cancelled")
		case <-deadline:
			return errResult("timeout waiting for child")
		case <-ticker.C:
			proc, err := h.king.Registry().Get(req.TargetPid)
			if err != nil {
				// Process already removed from registry.
				return okResult(req.TargetPid, -1, "")
			}
			if proc.State == process.StateZombie || proc.State == process.StateDead {
				result, _ := h.king.Lifecycle().WaitResult(proc.PID)
				if result != nil {
					return okResult(proc.PID, int32(result.ExitCode), string(result.Output))
				}
				return okResult(proc.PID, -1, "")
			}
		}
	}
}

