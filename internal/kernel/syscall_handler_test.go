package kernel

import (
	"context"
	"net"
	"testing"

	pb "github.com/selemilka/hivekernel/api/proto/hivepb"
	"github.com/selemilka/hivekernel/internal/runtime"

	"google.golang.org/grpc"
)

func newTestKingForSyscall(t *testing.T) *King {
	t.Helper()
	cfg := DefaultConfig()
	cfg.NodeName = "test-vps"
	king, err := New(cfg)
	if err != nil {
		t.Fatalf("new king: %v", err)
	}
	return king
}

func TestSyscallHandler_Spawn(t *testing.T) {
	king := newTestKingForSyscall(t)
	handler := NewKernelSyscallHandler(king, nil)

	result := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "test-spawn-1",
		Call: &pb.SystemCall_Spawn{
			Spawn: &pb.SpawnRequest{
				Name:          "test-child",
				Role:          pb.AgentRole_ROLE_WORKER,
				CognitiveTier: pb.CognitiveTier_COG_TACTICAL,
				Model:         "sonnet",
			},
		},
	})

	if result.CallId != "test-spawn-1" {
		t.Errorf("call_id = %q, want %q", result.CallId, "test-spawn-1")
	}
	spawn := result.GetSpawn()
	if spawn == nil {
		t.Fatal("expected SpawnResponse")
	}
	if !spawn.Success {
		t.Fatalf("spawn failed: %s", spawn.Error)
	}
	if spawn.ChildPid == 0 {
		t.Error("child_pid should be > 0")
	}
}

func TestSyscallHandler_Kill(t *testing.T) {
	king := newTestKingForSyscall(t)
	handler := NewKernelSyscallHandler(king, nil)

	// Spawn a child to kill.
	spawnResult := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "spawn-for-kill",
		Call: &pb.SystemCall_Spawn{
			Spawn: &pb.SpawnRequest{
				Name:          "to-kill",
				Role:          pb.AgentRole_ROLE_TASK,
				CognitiveTier: pb.CognitiveTier_COG_OPERATIONAL,
			},
		},
	})
	childPID := spawnResult.GetSpawn().ChildPid

	// Kill it.
	result := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "test-kill-1",
		Call: &pb.SystemCall_Kill{
			Kill: &pb.KillRequest{
				TargetPid: childPID,
				Recursive: true,
			},
		},
	})

	kill := result.GetKill()
	if kill == nil {
		t.Fatal("expected KillResponse")
	}
	if !kill.Success {
		t.Fatalf("kill failed: %s", kill.Error)
	}
	found := false
	for _, pid := range kill.KilledPids {
		if pid == childPID {
			found = true
		}
	}
	if !found {
		t.Errorf("killed_pids %v does not contain %d", kill.KilledPids, childPID)
	}
}

func TestSyscallHandler_KillNotOwn(t *testing.T) {
	king := newTestKingForSyscall(t)
	handler := NewKernelSyscallHandler(king, nil)

	// Spawn a child under king.
	spawnResult := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "spawn-1",
		Call: &pb.SystemCall_Spawn{
			Spawn: &pb.SpawnRequest{
				Name:          "child-a",
				Role:          pb.AgentRole_ROLE_WORKER,
				CognitiveTier: pb.CognitiveTier_COG_TACTICAL,
			},
		},
	})
	childPID := spawnResult.GetSpawn().ChildPid

	// Try to kill from a different PID (not the parent) — should fail.
	result := handler.HandleSyscall(context.Background(), 9999, &pb.SystemCall{
		CallId: "bad-kill",
		Call: &pb.SystemCall_Kill{
			Kill: &pb.KillRequest{TargetPid: childPID},
		},
	})

	kill := result.GetKill()
	if kill.Success {
		t.Error("kill should fail when caller is not parent")
	}
}

func TestSyscallHandler_Send(t *testing.T) {
	king := newTestKingForSyscall(t)
	handler := NewKernelSyscallHandler(king, nil)

	// Spawn a child so we have a sender.
	spawnResult := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "spawn-sender",
		Call: &pb.SystemCall_Spawn{
			Spawn: &pb.SpawnRequest{
				Name:          "sender",
				Role:          pb.AgentRole_ROLE_WORKER,
				CognitiveTier: pb.CognitiveTier_COG_TACTICAL,
			},
		},
	})
	senderPID := spawnResult.GetSpawn().ChildPid

	result := handler.HandleSyscall(context.Background(), senderPID, &pb.SystemCall{
		CallId: "test-send-1",
		Call: &pb.SystemCall_Send{
			Send: &pb.SendMessageRequest{
				ToPid:   king.PID(),
				Type:    "hello",
				Payload: []byte("test message"),
			},
		},
	})

	send := result.GetSend()
	if send == nil {
		t.Fatal("expected SendMessageResponse")
	}
	if !send.Delivered {
		t.Fatalf("send failed: %s", send.Error)
	}
	if send.MessageId == "" {
		t.Error("message_id should not be empty")
	}
}

func TestSyscallHandler_StoreAndGetArtifact(t *testing.T) {
	king := newTestKingForSyscall(t)
	handler := NewKernelSyscallHandler(king, nil)

	// Store.
	storeResult := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "test-store-1",
		Call: &pb.SystemCall_Store{
			Store: &pb.StoreArtifactRequest{
				Key:         "test/data.txt",
				Content:     []byte("hello artifact"),
				ContentType: "text/plain",
				Visibility:  pb.ArtifactVisibility_VIS_GLOBAL,
			},
		},
	})

	store := storeResult.GetStore()
	if store == nil || !store.Success {
		t.Fatalf("store failed: %v", store)
	}
	if store.ArtifactId == "" {
		t.Error("artifact_id should not be empty")
	}

	// Get.
	getResult := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "test-get-1",
		Call: &pb.SystemCall_GetArtifact{
			GetArtifact: &pb.GetArtifactRequest{Key: "test/data.txt"},
		},
	})

	get := getResult.GetGetArtifact()
	if get == nil || !get.Found {
		t.Fatalf("get failed: %v", get)
	}
	if string(get.Content) != "hello artifact" {
		t.Errorf("content = %q, want %q", string(get.Content), "hello artifact")
	}
}

func TestSyscallHandler_Escalate(t *testing.T) {
	king := newTestKingForSyscall(t)
	handler := NewKernelSyscallHandler(king, nil)

	result := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "test-escalate-1",
		Call: &pb.SystemCall_Escalate{
			Escalate: &pb.EscalateRequest{
				Issue:    "test issue",
				Severity: pb.EscalationSeverity_ESC_WARNING,
			},
		},
	})

	esc := result.GetEscalate()
	if esc == nil || !esc.Received {
		t.Fatalf("escalate failed: %v", esc)
	}
}

func TestSyscallHandler_Log(t *testing.T) {
	king := newTestKingForSyscall(t)
	handler := NewKernelSyscallHandler(king, nil)

	result := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "test-log-1",
		Call: &pb.SystemCall_Log{
			Log: &pb.LogRequest{
				Level:   pb.LogLevel_LOG_INFO,
				Message: "test log message",
			},
		},
	})

	if result.CallId != "test-log-1" {
		t.Errorf("call_id = %q, want %q", result.CallId, "test-log-1")
	}
	logResp := result.GetLog()
	if logResp == nil {
		t.Fatal("expected LogResponse")
	}
}

// --- execute_on tests ---

// mockAgentForExecOn implements AgentService Execute for testing execute_on.
type mockAgentForExecOn struct {
	pb.UnimplementedAgentServiceServer
}

func (m *mockAgentForExecOn) Execute(stream pb.AgentService_ExecuteServer) error {
	input, err := stream.Recv()
	if err != nil {
		return err
	}
	task := input.GetTask()
	if task == nil {
		return nil
	}
	return stream.Send(&pb.TaskProgress{
		TaskId:  task.TaskId,
		Type:    pb.ProgressType_PROGRESS_COMPLETED,
		Message: "done",
		Result: &pb.TaskResult{
			ExitCode: 0,
			Output:   "exec-on: " + task.Description,
		},
	})
}

func TestSyscallHandler_ExecuteOn(t *testing.T) {
	king := newTestKingForSyscall(t)

	// Start mock agent gRPC server.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	pb.RegisterAgentServiceServer(srv, &mockAgentForExecOn{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.GracefulStop)
	agentAddr := lis.Addr().String()

	// Create manager and register a runtime entry pointing to our mock agent.
	mgr := runtime.NewManager("localhost:50051", "python")
	handler := NewKernelSyscallHandler(king, mgr)
	executor := runtime.NewExecutor(handler)
	handler.SetExecutor(executor)

	// Spawn a child under king.
	spawnResult := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "spawn-for-execute-on",
		Call: &pb.SystemCall_Spawn{
			Spawn: &pb.SpawnRequest{
				Name:          "exec-target",
				Role:          pb.AgentRole_ROLE_WORKER,
				CognitiveTier: pb.CognitiveTier_COG_TACTICAL,
			},
		},
	})
	childPID := spawnResult.GetSpawn().ChildPid

	// Register a virtual runtime for the child, then overwrite Addr to mock agent.
	childProc, _ := king.Registry().Get(childPID)
	_, err = mgr.StartRuntime(childProc, runtime.RuntimePython)
	if err != nil {
		t.Fatalf("register runtime: %v", err)
	}
	rt := mgr.GetRuntime(childPID)
	rt.Addr = agentAddr

	// Execute_on: king sends a task to child.
	result := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "test-execute-on-1",
		Call: &pb.SystemCall_ExecuteOn{
			ExecuteOn: &pb.ExecuteOnRequest{
				TargetPid: childPID,
				Task: &pb.TaskRequest{
					TaskId:      "task-exec-on",
					Description: "hello from parent",
				},
			},
		},
	})

	execOn := result.GetExecuteOn()
	if execOn == nil {
		t.Fatal("expected ExecuteOnResponse")
	}
	if !execOn.Success {
		t.Fatalf("execute_on failed: %s", execOn.Error)
	}
	if execOn.Result == nil {
		t.Fatal("expected TaskResult")
	}
	if execOn.Result.Output != "exec-on: hello from parent" {
		t.Errorf("output = %q, want %q", execOn.Result.Output, "exec-on: hello from parent")
	}
}

func TestSyscallHandler_ExecuteOn_NotOwnChild(t *testing.T) {
	king := newTestKingForSyscall(t)
	mgr := runtime.NewManager("localhost:50051", "python")
	handler := NewKernelSyscallHandler(king, mgr)

	// Spawn a child under king.
	spawnResult := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "spawn-1",
		Call: &pb.SystemCall_Spawn{
			Spawn: &pb.SpawnRequest{
				Name:          "child-x",
				Role:          pb.AgentRole_ROLE_WORKER,
				CognitiveTier: pb.CognitiveTier_COG_TACTICAL,
			},
		},
	})
	childPID := spawnResult.GetSpawn().ChildPid

	// Try execute_on from a different PID (not the parent).
	result := handler.HandleSyscall(context.Background(), 9999, &pb.SystemCall{
		CallId: "bad-execute-on",
		Call: &pb.SystemCall_ExecuteOn{
			ExecuteOn: &pb.ExecuteOnRequest{
				TargetPid: childPID,
				Task: &pb.TaskRequest{
					TaskId:      "task-bad",
					Description: "should fail",
				},
			},
		},
	})

	execOn := result.GetExecuteOn()
	if execOn.Success {
		t.Error("execute_on should fail when caller is not parent")
	}
}

func TestSyscallHandler_ExecuteOn_NoRuntime(t *testing.T) {
	king := newTestKingForSyscall(t)
	mgr := runtime.NewManager("localhost:50051", "python")
	handler := NewKernelSyscallHandler(king, mgr)
	executor := runtime.NewExecutor(handler)
	handler.SetExecutor(executor)

	// Spawn a child but don't register a runtime.
	spawnResult := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "spawn-no-rt",
		Call: &pb.SystemCall_Spawn{
			Spawn: &pb.SpawnRequest{
				Name:          "child-no-rt",
				Role:          pb.AgentRole_ROLE_WORKER,
				CognitiveTier: pb.CognitiveTier_COG_TACTICAL,
			},
		},
	})
	childPID := spawnResult.GetSpawn().ChildPid

	result := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "exec-on-no-rt",
		Call: &pb.SystemCall_ExecuteOn{
			ExecuteOn: &pb.ExecuteOnRequest{
				TargetPid: childPID,
				Task: &pb.TaskRequest{
					TaskId:      "task-no-rt",
					Description: "should fail",
				},
			},
		},
	})

	execOn := result.GetExecuteOn()
	if execOn.Success {
		t.Error("execute_on should fail when no runtime is registered")
	}
}
