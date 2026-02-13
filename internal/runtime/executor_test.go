package runtime

import (
	"context"
	"net"
	"testing"

	pb "github.com/selemilka/hivekernel/api/proto/hivepb"
	"github.com/selemilka/hivekernel/internal/process"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// mockSyscallHandler records calls and returns fixed results.
type mockSyscallHandler struct {
	calls []*pb.SystemCall
}

func (m *mockSyscallHandler) HandleSyscall(ctx context.Context, callerPID process.PID, call *pb.SystemCall) *pb.SyscallResult {
	m.calls = append(m.calls, call)

	switch c := call.Call.(type) {
	case *pb.SystemCall_Spawn:
		_ = c
		return &pb.SyscallResult{
			CallId: call.CallId,
			Result: &pb.SyscallResult_Spawn{
				Spawn: &pb.SpawnResponse{Success: true, ChildPid: 42},
			},
		}
	case *pb.SystemCall_Log:
		_ = c
		return &pb.SyscallResult{
			CallId: call.CallId,
			Result: &pb.SyscallResult_Log{
				Log: &pb.LogResponse{},
			},
		}
	default:
		return &pb.SyscallResult{CallId: call.CallId}
	}
}

// mockAgentServicer is a test agent that handles Execute streams.
type mockAgentServicer struct {
	pb.UnimplementedAgentServiceServer
	taskHandler func(task *pb.TaskRequest, stream pb.AgentService_ExecuteServer) error
}

func (m *mockAgentServicer) Execute(stream pb.AgentService_ExecuteServer) error {
	// Read the first message (should be TaskRequest).
	input, err := stream.Recv()
	if err != nil {
		return err
	}

	task := input.GetTask()
	if task == nil {
		return nil
	}

	if m.taskHandler != nil {
		return m.taskHandler(task, stream)
	}

	// Default: immediately complete.
	return stream.Send(&pb.TaskProgress{
		TaskId:  task.TaskId,
		Type:    pb.ProgressType_PROGRESS_COMPLETED,
		Message: "done",
		Result: &pb.TaskResult{
			ExitCode: 0,
			Output:   "echo: " + task.Description,
		},
	})
}

func startMockAgent(t *testing.T, servicer *mockAgentServicer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	pb.RegisterAgentServiceServer(srv, servicer)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.GracefulStop)
	return lis.Addr().String()
}

func TestExecuteTask_SimpleComplete(t *testing.T) {
	handler := &mockSyscallHandler{}
	executor := NewExecutor(handler)

	addr := startMockAgent(t, &mockAgentServicer{})

	result, err := executor.ExecuteTask(
		context.Background(),
		addr,
		1,
		&pb.TaskRequest{
			TaskId:      "task-1",
			Description: "hello world",
		},
	)
	if err != nil {
		t.Fatalf("ExecuteTask: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	if result.Output != "echo: hello world" {
		t.Errorf("output = %q, want %q", result.Output, "echo: hello world")
	}
}

func TestExecuteTask_WithSyscall(t *testing.T) {
	handler := &mockSyscallHandler{}
	executor := NewExecutor(handler)

	servicer := &mockAgentServicer{
		taskHandler: func(task *pb.TaskRequest, stream pb.AgentService_ExecuteServer) error {
			// Agent sends a syscall (spawn).
			if err := stream.Send(&pb.TaskProgress{
				TaskId: task.TaskId,
				Type:   pb.ProgressType_PROGRESS_SYSCALL,
				Syscalls: []*pb.SystemCall{
					{
						CallId: "call-1",
						Call: &pb.SystemCall_Spawn{
							Spawn: &pb.SpawnRequest{
								Name: "child-1",
								Role: pb.AgentRole_ROLE_TASK,
							},
						},
					},
				},
			}); err != nil {
				return err
			}

			// Read the syscall result from the core.
			input, err := stream.Recv()
			if err != nil {
				return err
			}
			result := input.GetSyscallResult()
			if result == nil {
				t.Error("expected SyscallResult, got nil")
			}
			if result.GetSpawn().ChildPid != 42 {
				t.Errorf("child_pid = %d, want 42", result.GetSpawn().ChildPid)
			}

			// Complete the task.
			return stream.Send(&pb.TaskProgress{
				TaskId:  task.TaskId,
				Type:    pb.ProgressType_PROGRESS_COMPLETED,
				Message: "done",
				Result:  &pb.TaskResult{ExitCode: 0, Output: "spawned child 42"},
			})
		},
	}

	addr := startMockAgent(t, servicer)

	result, err := executor.ExecuteTask(
		context.Background(),
		addr,
		1,
		&pb.TaskRequest{TaskId: "task-2", Description: "spawn test"},
	)
	if err != nil {
		t.Fatalf("ExecuteTask: %v", err)
	}
	if result.Output != "spawned child 42" {
		t.Errorf("output = %q, want %q", result.Output, "spawned child 42")
	}
	if len(handler.calls) != 1 {
		t.Errorf("syscall calls = %d, want 1", len(handler.calls))
	}
}

func TestExecuteTask_Failed(t *testing.T) {
	handler := &mockSyscallHandler{}
	executor := NewExecutor(handler)

	servicer := &mockAgentServicer{
		taskHandler: func(task *pb.TaskRequest, stream pb.AgentService_ExecuteServer) error {
			return stream.Send(&pb.TaskProgress{
				TaskId:  task.TaskId,
				Type:    pb.ProgressType_PROGRESS_FAILED,
				Message: "something went wrong",
				Result:  &pb.TaskResult{ExitCode: 1, Output: "error"},
			})
		},
	}

	addr := startMockAgent(t, servicer)

	result, err := executor.ExecuteTask(
		context.Background(),
		addr,
		1,
		&pb.TaskRequest{TaskId: "task-3", Description: "fail test"},
	)
	if err == nil {
		t.Fatal("expected error for failed task")
	}
	if result == nil {
		t.Fatal("expected result even on failure")
	}
	if result.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", result.ExitCode)
	}
}

func TestExecuteTask_DialError(t *testing.T) {
	handler := &mockSyscallHandler{}
	executor := NewExecutor(handler)

	// Connect to non-existent address using passthrough to avoid DNS resolution.
	_, err := executor.ExecuteTask(
		context.Background(),
		"passthrough:///127.0.0.1:1", // port 1 is unlikely to be open
		1,
		&pb.TaskRequest{TaskId: "task-4", Description: "dial test"},
	)
	if err == nil {
		t.Fatal("expected error for bad address")
	}
}

// Ensure SyscallHandler interface is implemented.
var _ SyscallHandler = (*mockSyscallHandler)(nil)

// Suppress unused import warning.
var _ = insecure.NewCredentials
