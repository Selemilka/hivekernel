package kernel

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	pb "github.com/selemilka/hivekernel/api/proto/hivepb"
	"github.com/selemilka/hivekernel/internal/hklog"
	"github.com/selemilka/hivekernel/internal/ipc"
	"github.com/selemilka/hivekernel/internal/permissions"
	"github.com/selemilka/hivekernel/internal/process"
	"github.com/selemilka/hivekernel/internal/resources"
	"github.com/selemilka/hivekernel/internal/runtime"
	"github.com/selemilka/hivekernel/internal/scheduler"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// CoreServer implements the gRPC CoreService.
// Each agent connects to this server to use system capabilities.
type CoreServer struct {
	pb.UnimplementedCoreServiceServer
	king     *King
	executor *runtime.Executor
}

// NewCoreServer creates a CoreService gRPC server backed by the king.
func NewCoreServer(king *King) *CoreServer {
	return &CoreServer{king: king}
}

// SetExecutor sets the executor for task execution (called after creation).
func (s *CoreServer) SetExecutor(e *runtime.Executor) {
	s.executor = e
}

// Register registers the CoreService on a gRPC server.
func (s *CoreServer) Register(srv *grpc.Server) {
	pb.RegisterCoreServiceServer(srv, s)
}

// callerPID extracts the caller's PID from gRPC metadata.
// Agents send their PID in the "x-hivekernel-pid" header.
func callerPID(ctx context.Context) (process.PID, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return 0, status.Error(codes.Unauthenticated, "missing metadata")
	}
	vals := md.Get("x-hivekernel-pid")
	if len(vals) == 0 {
		return 0, status.Error(codes.Unauthenticated, "missing x-hivekernel-pid")
	}
	var pid uint64
	_, err := fmt.Sscanf(vals[0], "%d", &pid)
	if err != nil {
		return 0, status.Errorf(codes.InvalidArgument, "invalid pid: %s", vals[0])
	}
	return pid, nil
}

// --- Process management ---

func (s *CoreServer) SpawnChild(ctx context.Context, req *pb.SpawnRequest) (*pb.SpawnResponse, error) {
	parentPID, err := callerPID(ctx)
	if err != nil {
		return nil, err
	}

	proc, err := s.king.SpawnChild(process.SpawnRequest{
		ParentPID:     parentPID,
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
		return &pb.SpawnResponse{Success: false, Error: err.Error()}, nil
	}

	hklog.For("grpc").Info("SpawnChild", "parent_pid", parentPID, "name", proc.Name, "child_pid", proc.PID)
	return &pb.SpawnResponse{Success: true, ChildPid: proc.PID}, nil
}

func (s *CoreServer) KillChild(ctx context.Context, req *pb.KillRequest) (*pb.KillResponse, error) {
	parentPID, err := callerPID(ctx)
	if err != nil {
		return nil, err
	}

	target, err := s.king.Registry().Get(req.TargetPid)
	if err != nil {
		return &pb.KillResponse{Success: false, Error: "process not found"}, nil
	}

	// Verify caller is the parent.
	if target.PPID != parentPID {
		return &pb.KillResponse{Success: false, Error: "can only kill own children"}, nil
	}

	var killed []uint64

	if req.Recursive {
		// Kill descendants first.
		descendants := s.king.Registry().GetDescendants(req.TargetPid)
		for _, d := range descendants {
			if s.king.RuntimeManager() != nil {
				_ = s.king.RuntimeManager().StopRuntime(d.PID)
			}
			_ = s.king.Registry().SetState(d.PID, process.StateZombie)
			s.king.Signals().NotifyParent(d.PID, -1, "killed")
			killed = append(killed, d.PID)
		}
	}

	if s.king.RuntimeManager() != nil {
		_ = s.king.RuntimeManager().StopRuntime(req.TargetPid)
	}
	_ = s.king.Registry().SetState(req.TargetPid, process.StateZombie)
	s.king.Signals().NotifyParent(req.TargetPid, -1, "killed")
	killed = append(killed, req.TargetPid)

	hklog.For("grpc").Info("KillChild", "parent_pid", parentPID, "killed", killed)
	return &pb.KillResponse{Success: true, KilledPids: killed}, nil
}

func (s *CoreServer) GetProcessInfo(ctx context.Context, req *pb.ProcessInfoRequest) (*pb.ProcessInfo, error) {
	pid := req.Pid
	if pid == 0 {
		// pid=0 means "self"
		var err error
		pid, err = callerPID(ctx)
		if err != nil {
			return nil, err
		}
	}

	proc, err := s.king.Registry().Get(pid)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "process %d not found", pid)
	}

	info := processToProto(proc)
	info.ChildCount = uint32(s.king.Registry().CountChildren(pid))
	return info, nil
}

func (s *CoreServer) ListChildren(ctx context.Context, req *pb.ListChildrenRequest) (*pb.ListChildrenResponse, error) {
	parentPID, err := callerPID(ctx)
	if err != nil {
		return nil, err
	}

	var procs []*process.Process
	if req.Recursive {
		procs = s.king.Registry().GetDescendants(parentPID)
	} else {
		procs = s.king.Registry().GetChildren(parentPID)
	}

	resp := &pb.ListChildrenResponse{}
	for _, p := range procs {
		resp.Children = append(resp.Children, processToProto(p))
	}
	return resp, nil
}

// --- IPC ---

func (s *CoreServer) SendMessage(ctx context.Context, req *pb.SendMessageRequest) (*pb.SendMessageResponse, error) {
	fromPID, err := callerPID(ctx)
	if err != nil {
		return nil, err
	}

	// ACL check: does the caller have permission to send messages?
	if err := s.king.ACL().Check(fromPID, permissions.ActionSendMessage); err != nil {
		return &pb.SendMessageResponse{Delivered: false, Error: err.Error()}, nil
	}

	// Rate limit check.
	if !s.king.RateLimiter().Allow(fromPID) {
		return &pb.SendMessageResponse{Delivered: false, Error: "rate limit exceeded"}, nil
	}

	sender, err := s.king.Registry().Get(fromPID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "sender %d not found", fromPID)
	}

	msg := &ipc.Message{
		FromPID:     fromPID,
		FromName:    sender.Name,
		ToPID:       req.ToPid,
		ToQueue:     req.ToQueue,
		Type:        req.Type,
		Priority:    int(req.Priority),
		Payload:     req.Payload,
		RequiresAck: req.RequiresAck,
		ReplyTo:     req.ReplyTo,
	}

	// Route through broker (validates routing rules, computes priority).
	if err := s.king.Broker().Route(msg); err != nil {
		return &pb.SendMessageResponse{Delivered: false, Error: err.Error()}, nil
	}

	hklog.For("grpc").Debug("SendMessage", "from_pid", fromPID, "to_pid", req.ToPid, "type", req.Type)

	// Extract trace context from payload and propagate to receiver.
	traceID, traceSpan := extractTraceFromPayload(req.Payload)
	if traceID != "" {
		s.king.SetTrace(req.ToPid, traceID, traceSpan)
	}

	// Emit message_sent event for dashboard visualization.
	if el := s.king.EventLog(); el != nil {
		toName := ""
		if receiver, err := s.king.Registry().Get(req.ToPid); err == nil {
			toName = receiver.Name
		}
		el.Emit(process.ProcessEvent{
			Type:           process.EventMessageSent,
			PID:            fromPID,
			PPID:           req.ToPid,
			Name:           sender.Name,
			Role:           toName,
			Message:        req.Type,
			ReplyTo:        req.ReplyTo,
			PayloadPreview: truncatePayload(string(req.Payload), 2000),
			TraceID:        traceID,
			TraceSpan:      traceSpan,
			MessageID:      msg.ID,
		})
	}

	return &pb.SendMessageResponse{Delivered: true, MessageId: msg.ID}, nil
}

func (s *CoreServer) Subscribe(req *pb.SubscribeRequest, stream grpc.ServerStreamingServer[pb.AgentMessage]) error {
	// Get caller PID from stream context.
	pid, err := callerPID(stream.Context())
	if err != nil {
		return err
	}

	// Get or create the named queue / process inbox.
	var q *ipc.PriorityQueue
	if req.Queue != "" {
		q = s.king.Broker().GetNamedQueue(req.Queue)
	} else {
		q = s.king.Broker().GetInbox(pid)
	}

	hklog.For("grpc").Info("Subscribe", "pid", pid, "queue", req.Queue)

	// Stream messages until client disconnects.
	ctx := stream.Context()
	for {
		msg := q.PopWait(ctx.Done())
		if msg == nil {
			return nil // context cancelled
		}

		// Apply type filter if set.
		if req.TypeFilter != "" && msg.Type != req.TypeFilter {
			q.Push(msg) // put it back
			continue
		}

		pbMsg := &pb.AgentMessage{
			MessageId:   msg.ID,
			FromPid:     msg.FromPID,
			FromName:    msg.FromName,
			Type:        msg.Type,
			Priority:    pb.Priority(msg.Priority),
			Payload:     msg.Payload,
			RequiresAck: msg.RequiresAck,
			ReplyTo:     msg.ReplyTo,
		}

		if err := stream.Send(pbMsg); err != nil {
			return err
		}
	}
}

// --- Shared memory ---

func (s *CoreServer) StoreArtifact(ctx context.Context, req *pb.StoreArtifactRequest) (*pb.StoreArtifactResponse, error) {
	pid, err := callerPID(ctx)
	if err != nil {
		return nil, err
	}

	// ACL check.
	if err := s.king.ACL().Check(pid, permissions.ActionWriteArtifact); err != nil {
		return &pb.StoreArtifactResponse{Success: false, Error: err.Error()}, nil
	}

	vis := ipc.Visibility(req.Visibility)
	id, err := s.king.SharedMemory().Store(pid, req.Key, req.Content, req.ContentType, vis)
	if err != nil {
		return &pb.StoreArtifactResponse{Success: false, Error: err.Error()}, nil
	}

	hklog.For("grpc").Debug("StoreArtifact", "pid", pid, "key", req.Key, "id", id, "visibility", vis)
	return &pb.StoreArtifactResponse{Success: true, ArtifactId: id}, nil
}

func (s *CoreServer) GetArtifact(ctx context.Context, req *pb.GetArtifactRequest) (*pb.GetArtifactResponse, error) {
	pid, err := callerPID(ctx)
	if err != nil {
		return nil, err
	}

	// Try by key first, then by ID.
	var art *ipc.Artifact
	if req.Key != "" {
		art, err = s.king.SharedMemory().Get(pid, req.Key)
	} else {
		art, err = s.king.SharedMemory().GetByID(pid, req.ArtifactId)
	}
	if err != nil {
		return &pb.GetArtifactResponse{Found: false, Error: err.Error()}, nil
	}

	return &pb.GetArtifactResponse{
		Found:       true,
		Content:     art.Content,
		ContentType: art.ContentType,
		StoredByPid: art.StoredByPID,
		StoredAt:    uint64(art.StoredAt.Unix()),
	}, nil
}

func (s *CoreServer) ListArtifacts(ctx context.Context, req *pb.ListArtifactsRequest) (*pb.ListArtifactsResponse, error) {
	pid, err := callerPID(ctx)
	if err != nil {
		return nil, err
	}

	arts := s.king.SharedMemory().List(pid, req.Prefix)
	resp := &pb.ListArtifactsResponse{}
	for _, a := range arts {
		resp.Artifacts = append(resp.Artifacts, &pb.ArtifactMeta{
			ArtifactId:  a.ID,
			Key:         a.Key,
			ContentType: a.ContentType,
			SizeBytes:   uint64(len(a.Content)),
			StoredByPid: a.StoredByPID,
			StoredAt:    uint64(a.StoredAt.Unix()),
		})
	}
	return resp, nil
}

// --- Resources ---

func (s *CoreServer) GetResourceUsage(ctx context.Context, req *pb.ResourceUsageRequest) (*pb.ResourceUsage, error) {
	pid, err := callerPID(ctx)
	if err != nil {
		return nil, err
	}

	proc, err := s.king.Registry().Get(pid)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "process %d not found", pid)
	}

	// Get budget-aware remaining tokens.
	tier := resources.TierFromCog(proc.CognitiveTier)
	var tokensRemaining uint64
	if b := s.king.Budget().GetBudget(pid, tier); b != nil {
		tokensRemaining = b.Remaining()
	} else {
		// Fallback to static limit.
		if proc.Limits.MaxTokensTotal > proc.TokensConsumed {
			tokensRemaining = proc.Limits.MaxTokensTotal - proc.TokensConsumed
		}
	}

	var uptimeSeconds uint64
	if !proc.StartedAt.IsZero() {
		uptimeSeconds = uint64(proc.UpdatedAt.Sub(proc.StartedAt).Seconds())
	}

	return &pb.ResourceUsage{
		TokensConsumed:      proc.TokensConsumed,
		TokensRemaining:     tokensRemaining,
		ChildrenActive:      uint32(s.king.Registry().CountChildren(pid)),
		ChildrenMax:         proc.Limits.MaxChildren,
		ContextUsagePercent: proc.ContextUsagePercent,
		UptimeSeconds:       uptimeSeconds,
	}, nil
}

func (s *CoreServer) RequestResources(ctx context.Context, req *pb.ResourceRequest) (*pb.ResourceResponse, error) {
	pid, err := callerPID(ctx)
	if err != nil {
		return nil, err
	}

	proc, err := s.king.Registry().Get(pid)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "process %d not found", pid)
	}

	tier := resources.TierFromCog(proc.CognitiveTier)

	// Try to allocate additional tokens from parent's budget.
	err = s.king.Budget().Allocate(proc.PPID, pid, tier, req.Amount)
	if err != nil {
		hklog.For("grpc").Warn("RequestResources denied", "pid", pid, "error", err)
		return &pb.ResourceResponse{
			Granted: false,
			Reason:  err.Error(),
		}, nil
	}

	hklog.For("grpc").Info("RequestResources granted", "pid", pid, "amount", req.Amount, "tier", tier)
	return &pb.ResourceResponse{
		Granted:       true,
		GrantedAmount: req.Amount,
	}, nil
}

// --- Escalation ---

func (s *CoreServer) Escalate(ctx context.Context, req *pb.EscalateRequest) (*pb.EscalateResponse, error) {
	fromPID, err := callerPID(ctx)
	if err != nil {
		return nil, err
	}

	hklog.For("grpc").Warn("Escalate", "from_pid", fromPID, "severity", req.Severity, "issue", req.Issue)

	// Push as a message to king's inbox.
	s.king.Inbox().Push(&ipc.Message{
		FromPID:  fromPID,
		ToPID:    s.king.PID(),
		Type:     "escalation",
		Priority: ipc.PriorityHigh,
		Payload:  []byte(req.Issue),
	})

	return &pb.EscalateResponse{Received: true, HandledByPid: s.king.PID()}, nil
}

// --- Logging ---

func (s *CoreServer) Log(ctx context.Context, req *pb.LogRequest) (*pb.LogResponse, error) {
	pid, _ := callerPID(ctx)
	hklog.For("grpc").Debug("agent log", "pid", pid, "level", req.Level, "message", req.Message)
	return &pb.LogResponse{}, nil
}

func (s *CoreServer) ReportMetric(ctx context.Context, req *pb.MetricRequest) (*pb.MetricResponse, error) {
	pid, _ := callerPID(ctx)
	hklog.For("grpc").Debug("metric reported", "pid", pid, "name", req.Name, "value", req.Value)

	// If the metric is token usage, record it in accounting and budget.
	if req.Name == "tokens_consumed" {
		tokens := uint64(req.Value)
		proc, err := s.king.Registry().Get(pid)
		if err == nil {
			tier := resources.TierFromCog(proc.CognitiveTier)
			s.king.Accountant().Record(pid, tier, tokens)

			if err := s.king.Budget().Consume(pid, tier, tokens); err != nil {
				hklog.For("grpc").Warn("budget exceeded", "pid", pid, "error", err)
				// Publish budget exceeded event.
				s.king.EventBus().Publish("budget_exceeded", pid,
					[]byte(fmt.Sprintf("PID %d exceeded %s budget", pid, tier)))
			}

			// Update process token counter.
			s.king.Registry().Update(pid, func(p *process.Process) {
				p.TokensConsumed += tokens
			})
		}
	}
	return &pb.MetricResponse{}, nil
}

// --- Task execution ---

func (s *CoreServer) ExecuteTask(ctx context.Context, req *pb.ExecuteTaskRequest) (*pb.ExecuteTaskResponse, error) {
	if s.executor == nil {
		return &pb.ExecuteTaskResponse{Success: false, Error: "executor not available"}, nil
	}
	if s.king.RuntimeManager() == nil {
		return &pb.ExecuteTaskResponse{Success: false, Error: "runtime manager not available"}, nil
	}

	target, err := s.king.Registry().Get(req.TargetPid)
	if err != nil {
		return &pb.ExecuteTaskResponse{Success: false, Error: "target process not found"}, nil
	}

	rt := s.king.RuntimeManager().GetRuntime(req.TargetPid)
	if rt == nil {
		return &pb.ExecuteTaskResponse{Success: false, Error: fmt.Sprintf("no runtime for PID %d", req.TargetPid)}, nil
	}

	// Extract trace context from params and propagate to target.
	if req.Params != nil {
		if tID, tSpan := extractTraceFromParams(req.Params); tID != "" {
			s.king.SetTrace(req.TargetPid, tID, tSpan)
		}
	}

	// Safely truncate description for task ID (preserve valid UTF-8).
	descForID := req.Description
	if utf8.RuneCountInString(descForID) > 20 {
		descForID = string([]rune(descForID)[:20])
	}
	taskReq := &pb.TaskRequest{
		TaskId:         fmt.Sprintf("ext-%d-%s", target.PID, descForID),
		Description:    req.Description,
		Params:         req.Params,
		TimeoutSeconds: uint32(req.TimeoutSeconds),
	}

	hklog.For("grpc").Info("ExecuteTask", "target_pid", req.TargetPid, "name", target.Name, "task", req.Description)
	result, err := s.executor.ExecuteTask(ctx, rt.Addr, req.TargetPid, taskReq)
	if err != nil {
		hklog.For("grpc").Error(fmt.Sprintf("ExecuteTask failed on %s (PID %d): %v", target.Name, req.TargetPid, err), "task", req.Description)
		return &pb.ExecuteTaskResponse{Success: false, Error: err.Error()}, nil
	}

	return &pb.ExecuteTaskResponse{Success: true, Result: result}, nil
}

// --- Cron management ---

func (s *CoreServer) AddCron(ctx context.Context, req *pb.AddCronRequest) (*pb.AddCronResponse, error) {
	sched, err := s.king.Cron().ParseAndAdd(req.Name, req.CronExpression, req.Action, process.PID(req.TargetPid), req.ExecuteDescription, req.ExecuteParams)
	if err != nil {
		return &pb.AddCronResponse{Error: err.Error()}, nil
	}
	hklog.For("grpc").Info("AddCron", "name", req.Name, "expr", req.CronExpression, "target_pid", req.TargetPid, "action", req.Action)
	return &pb.AddCronResponse{CronId: sched}, nil
}

func (s *CoreServer) RemoveCron(ctx context.Context, req *pb.RemoveCronRequest) (*pb.RemoveCronResponse, error) {
	if err := s.king.Cron().Remove(req.CronId); err != nil {
		return &pb.RemoveCronResponse{Ok: false, Error: err.Error()}, nil
	}
	hklog.For("grpc").Info("RemoveCron", "cron_id", req.CronId)
	return &pb.RemoveCronResponse{Ok: true}, nil
}

func (s *CoreServer) ListCron(ctx context.Context, req *pb.ListCronRequest) (*pb.ListCronResponse, error) {
	entries := s.king.Cron().List()
	now := time.Now()
	resp := &pb.ListCronResponse{}
	for _, e := range entries {
		var lastRunMs int64
		if !e.LastRun.IsZero() {
			lastRunMs = e.LastRun.UnixMilli()
		}
		nextRun := scheduler.NextRunAfter(e.Schedule, now)
		var nextRunMs int64
		if !nextRun.IsZero() {
			nextRunMs = nextRun.UnixMilli()
		}
		resp.Entries = append(resp.Entries, &pb.CronEntryProto{
			Id:                 e.ID,
			Name:               e.Name,
			CronExpression:     e.Schedule.Raw,
			Action:             e.Action.String(),
			TargetPid:          e.TargetPID,
			ExecuteDescription: e.ExecuteDesc,
			ExecuteParams:      e.ExecuteParams,
			Enabled:            e.Enabled,
			LastRunMs:           lastRunMs,
			NextRunMs:           nextRunMs,
			LastExitCode:        e.LastExitCode,
			LastOutput:          e.LastOutput,
			LastDurationMs:      e.LastDurationMs,
		})
	}
	return resp, nil
}

// --- Inbox inspection ---

func (s *CoreServer) ListInbox(ctx context.Context, req *pb.ListInboxRequest) (*pb.ListInboxResponse, error) {
	pid := process.PID(req.Pid)
	if pid == 0 {
		var err error
		pid, err = callerPID(ctx)
		if err != nil {
			return nil, err
		}
	}

	msgs := s.king.Broker().ListInbox(pid)
	resp := &pb.ListInboxResponse{
		Total: int32(len(msgs)),
	}
	for _, msg := range msgs {
		resp.Messages = append(resp.Messages, &pb.AgentMessage{
			MessageId:   msg.ID,
			FromPid:     msg.FromPID,
			FromName:    msg.FromName,
			Type:        msg.Type,
			Priority:    pb.Priority(msg.Priority),
			Payload:     msg.Payload,
			RequiresAck: msg.RequiresAck,
			ReplyTo:     msg.ReplyTo,
		})
	}
	return resp, nil
}

// --- Siblings / Wait ---

func (s *CoreServer) ListSiblings(ctx context.Context, req *pb.ListSiblingsRequest) (*pb.ListSiblingsResponse, error) {
	pid, err := callerPID(ctx)
	if err != nil {
		return nil, err
	}

	caller, err := s.king.Registry().Get(pid)
	if err != nil {
		return &pb.ListSiblingsResponse{}, nil
	}

	children := s.king.Registry().GetChildren(caller.PPID)
	var siblings []*pb.SiblingInfo
	for _, child := range children {
		if child.PID == pid {
			continue // exclude self
		}
		siblings = append(siblings, &pb.SiblingInfo{
			Pid:   child.PID,
			Name:  child.Name,
			Role:  child.Role.String(),
			State: child.State.String(),
		})
	}

	hklog.For("grpc").Debug("ListSiblings", "pid", pid, "count", len(siblings))
	return &pb.ListSiblingsResponse{Siblings: siblings}, nil
}

func (s *CoreServer) WaitChild(ctx context.Context, req *pb.WaitChildRequest) (*pb.WaitChildResponse, error) {
	pid, err := callerPID(ctx)
	if err != nil {
		return nil, err
	}

	// Validate target is a child of caller.
	target, err := s.king.Registry().Get(req.TargetPid)
	if err != nil {
		return &pb.WaitChildResponse{Success: false, Error: "process not found"}, nil
	}
	if target.PPID != pid {
		return &pb.WaitChildResponse{Success: false, Error: "can only wait for own children"}, nil
	}

	// If already zombie/dead, collect result immediately.
	if target.State == process.StateZombie || target.State == process.StateDead {
		result, _ := s.king.Lifecycle().WaitResult(target.PID)
		if result != nil {
			return &pb.WaitChildResponse{
				Success:  true,
				Pid:      target.PID,
				ExitCode: int32(result.ExitCode),
				Output:   string(result.Output),
			}, nil
		}
		return &pb.WaitChildResponse{Success: true, Pid: target.PID, ExitCode: -1}, nil
	}

	// Process is still alive -- poll until it dies or timeout.
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
			return &pb.WaitChildResponse{Success: false, Error: "context cancelled"}, nil
		case <-deadline:
			return &pb.WaitChildResponse{Success: false, Error: "timeout waiting for child"}, nil
		case <-ticker.C:
			proc, err := s.king.Registry().Get(req.TargetPid)
			if err != nil {
				return &pb.WaitChildResponse{Success: true, Pid: req.TargetPid, ExitCode: -1}, nil
			}
			if proc.State == process.StateZombie || proc.State == process.StateDead {
				result, _ := s.king.Lifecycle().WaitResult(proc.PID)
				if result != nil {
					return &pb.WaitChildResponse{
						Success:  true,
						Pid:      proc.PID,
						ExitCode: int32(result.ExitCode),
						Output:   string(result.Output),
					}, nil
				}
				return &pb.WaitChildResponse{Success: true, Pid: proc.PID, ExitCode: -1}, nil
			}
		}
	}
}

// --- Event sourcing ---

func (s *CoreServer) SubscribeEvents(req *pb.SubscribeEventsRequest, stream grpc.ServerStreamingServer[pb.ProcessEvent]) error {
	el := s.king.EventLog()
	if el == nil {
		return status.Error(codes.Unavailable, "event log not initialized")
	}

	ch := el.SubscribeSince(req.SinceSeq, 256)
	defer el.Unsubscribe(ch)

	ctx := stream.Context()
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return nil // channel closed (EventLog shutting down)
			}
			pbEvt := &pb.ProcessEvent{
				Seq:            evt.Seq,
				TimestampMs:    evt.Timestamp.UnixMilli(),
				Type:           string(evt.Type),
				Pid:            evt.PID,
				Ppid:           evt.PPID,
				Name:           evt.Name,
				Role:           evt.Role,
				Tier:           evt.Tier,
				Model:          evt.Model,
				State:          evt.State,
				OldState:       evt.OldState,
				NewState:       evt.NewState,
				Level:          evt.Level,
				Message:        evt.Message,
				ReplyTo:        evt.ReplyTo,
				PayloadPreview: evt.PayloadPreview,
				TraceId:        evt.TraceID,
				TraceSpan:      evt.TraceSpan,
				MessageId:      evt.MessageID,
				Fields:         evt.Fields,
			}
			if err := stream.Send(pbEvt); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
