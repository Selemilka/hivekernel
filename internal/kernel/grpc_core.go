package kernel

import (
	"context"
	"fmt"
	"log"

	pb "github.com/selemilka/hivekernel/api/proto/hivepb"
	"github.com/selemilka/hivekernel/internal/ipc"
	"github.com/selemilka/hivekernel/internal/permissions"
	"github.com/selemilka/hivekernel/internal/process"
	"github.com/selemilka/hivekernel/internal/resources"
	"github.com/selemilka/hivekernel/internal/runtime"

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
		RuntimeType:   req.RuntimeType.String(),
		RuntimeImage:  req.RuntimeImage,
	})
	if err != nil {
		return &pb.SpawnResponse{Success: false, Error: err.Error()}, nil
	}

	log.Printf("[grpc] SpawnChild: PID %d spawned %s (PID %d)", parentPID, proc.Name, proc.PID)
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

	log.Printf("[grpc] KillChild: PID %d killed %v", parentPID, killed)
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
	}

	// Route through broker (validates routing rules, computes priority).
	if err := s.king.Broker().Route(msg); err != nil {
		return &pb.SendMessageResponse{Delivered: false, Error: err.Error()}, nil
	}

	log.Printf("[grpc] SendMessage: PID %d -> PID %d, type=%s", fromPID, req.ToPid, req.Type)
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

	log.Printf("[grpc] Subscribe: PID %d subscribed to queue=%q", pid, req.Queue)

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
			MessageId: msg.ID,
			FromPid:   msg.FromPID,
			FromName:  msg.FromName,
			Type:      msg.Type,
			Priority:  pb.Priority(msg.Priority),
			Payload:   msg.Payload,
			RequiresAck: msg.RequiresAck,
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

	log.Printf("[grpc] StoreArtifact: PID %d stored %q (id=%s, vis=%s)", pid, req.Key, id, vis)
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
		log.Printf("[grpc] RequestResources: PID %d denied: %v", pid, err)
		return &pb.ResourceResponse{
			Granted: false,
			Reason:  err.Error(),
		}, nil
	}

	log.Printf("[grpc] RequestResources: PID %d granted %d %s tokens", pid, req.Amount, tier)
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

	log.Printf("[grpc] Escalate from PID %d: severity=%s issue=%s", fromPID, req.Severity, req.Issue)

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
	log.Printf("[agent:%d] [%s] %s", pid, req.Level, req.Message)
	return &pb.LogResponse{}, nil
}

func (s *CoreServer) ReportMetric(ctx context.Context, req *pb.MetricRequest) (*pb.MetricResponse, error) {
	pid, _ := callerPID(ctx)
	log.Printf("[metric:%d] %s = %f", pid, req.Name, req.Value)

	// If the metric is token usage, record it in accounting and budget.
	if req.Name == "tokens_consumed" {
		tokens := uint64(req.Value)
		proc, err := s.king.Registry().Get(pid)
		if err == nil {
			tier := resources.TierFromCog(proc.CognitiveTier)
			s.king.Accountant().Record(pid, tier, tokens)

			if err := s.king.Budget().Consume(pid, tier, tokens); err != nil {
				log.Printf("[metric:%d] budget exceeded: %v", pid, err)
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

	taskReq := &pb.TaskRequest{
		TaskId:         fmt.Sprintf("ext-%d-%s", target.PID, req.Description[:min(len(req.Description), 20)]),
		Description:    req.Description,
		Params:         req.Params,
		TimeoutSeconds: uint32(req.TimeoutSeconds),
	}

	log.Printf("[grpc] ExecuteTask: target PID %d (%s), task=%s", req.TargetPid, target.Name, req.Description)
	result, err := s.executor.ExecuteTask(ctx, rt.Addr, req.TargetPid, taskReq)
	if err != nil {
		return &pb.ExecuteTaskResponse{Success: false, Error: err.Error()}, nil
	}

	return &pb.ExecuteTaskResponse{Success: true, Result: result}, nil
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
				Seq:         evt.Seq,
				TimestampMs: evt.Timestamp.UnixMilli(),
				Type:        string(evt.Type),
				Pid:         evt.PID,
				Ppid:        evt.PPID,
				Name:        evt.Name,
				Role:        evt.Role,
				Tier:        evt.Tier,
				Model:       evt.Model,
				State:       evt.State,
				OldState:    evt.OldState,
				NewState:    evt.NewState,
			}
			if err := stream.Send(pbEvt); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
