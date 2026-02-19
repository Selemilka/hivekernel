package runtime

import (
	"context"
	"fmt"
	"io"
	pb "github.com/selemilka/hivekernel/api/proto/hivepb"
	"github.com/selemilka/hivekernel/internal/hklog"
	"github.com/selemilka/hivekernel/internal/process"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// SyscallHandler dispatches syscalls from agent streams to kernel subsystems.
type SyscallHandler interface {
	HandleSyscall(ctx context.Context, callerPID process.PID, call *pb.SystemCall) *pb.SyscallResult
}

// Executor manages Execute bidirectional streams with agent runtimes.
type Executor struct {
	handler SyscallHandler
}

// NewExecutor creates an executor with the given syscall handler.
func NewExecutor(handler SyscallHandler) *Executor {
	return &Executor{handler: handler}
}

// ExecuteTask opens a bidi Execute stream to the agent at agentAddr,
// sends a TaskRequest, processes syscalls, and returns the final TaskResult.
func (e *Executor) ExecuteTask(
	ctx context.Context,
	agentAddr string,
	callerPID process.PID,
	task *pb.TaskRequest,
) (*pb.TaskResult, error) {
	// No client-side keepalive: these are task-scoped connections with active
	// streams and context deadlines. Aggressive keepalive pings (e.g. every 5s)
	// trigger the Python gRPC server's default enforcement policy (min 5min
	// between pings), causing GOAWAY and connection loss during long execute_on
	// syscalls that block the stream for 60-300s.
	conn, err := grpc.NewClient(agentAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("executor: dial %s: %w", agentAddr, err)
	}
	defer conn.Close()

	client := pb.NewAgentServiceClient(conn)
	stream, err := client.Execute(ctx)
	if err != nil {
		return nil, fmt.Errorf("executor: open Execute stream: %w", err)
	}

	// Send the task request.
	if err := stream.Send(&pb.ExecuteInput{
		Input: &pb.ExecuteInput_Task{Task: task},
	}); err != nil {
		return nil, fmt.Errorf("executor: send task: %w", err)
	}

	// Read progress messages in a loop.
	for {
		progress, err := stream.Recv()
		if err == io.EOF {
			return nil, fmt.Errorf("executor: stream closed without result")
		}
		if err != nil {
			return nil, fmt.Errorf("executor: recv: %w", err)
		}

		switch progress.Type {
		case pb.ProgressType_PROGRESS_SYSCALL:
			// Dispatch each syscall and send results back.
			for _, call := range progress.Syscalls {
				result := e.handler.HandleSyscall(ctx, callerPID, call)
				if err := stream.Send(&pb.ExecuteInput{
					Input: &pb.ExecuteInput_SyscallResult{SyscallResult: result},
				}); err != nil {
					return nil, fmt.Errorf("executor: send syscall result: %w", err)
				}
			}

		case pb.ProgressType_PROGRESS_COMPLETED:
			outPreview := ""
			if progress.Result != nil && progress.Result.Output != "" {
				outPreview = progress.Result.Output
				if len(outPreview) > 200 {
					outPreview = outPreview[:200] + "..."
				}
			}
			hklog.For("executor").Info("task completed", "task_id", progress.TaskId, "pid", callerPID, "output_preview", outPreview)
			_ = stream.CloseSend()
			return progress.Result, nil

		case pb.ProgressType_PROGRESS_FAILED:
			errMsg := progress.Message
			if errMsg == "" && progress.Result != nil {
				errMsg = progress.Result.Output
			}
			hklog.For("executor").Error(fmt.Sprintf("task failed: %s", errMsg), "task_id", progress.TaskId, "pid", callerPID)
			_ = stream.CloseSend()
			return progress.Result, fmt.Errorf("task failed: %s", errMsg)

		case pb.ProgressType_PROGRESS_UPDATE:
			hklog.For("executor").Debug("task progress", "task_id", progress.TaskId, "percent", progress.ProgressPercent, "message", progress.Message)

		case pb.ProgressType_PROGRESS_LOG:
			hklog.For("executor").Debug("task log", "task_id", progress.TaskId, "message", progress.Message)
		}
	}
}
