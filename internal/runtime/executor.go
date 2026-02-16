package runtime

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	pb "github.com/selemilka/hivekernel/api/proto/hivepb"
	"github.com/selemilka/hivekernel/internal/process"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
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
	conn, err := grpc.NewClient(agentAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                5 * time.Second, // ping every 5s
			Timeout:             2 * time.Second, // wait 2s for pong
			PermitWithoutStream: false,
		}),
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
			log.Printf("[executor] task %s completed", progress.TaskId)
			_ = stream.CloseSend()
			return progress.Result, nil

		case pb.ProgressType_PROGRESS_FAILED:
			log.Printf("[executor] task %s failed: %s", progress.TaskId, progress.Message)
			_ = stream.CloseSend()
			return progress.Result, fmt.Errorf("task failed: %s", progress.Message)

		case pb.ProgressType_PROGRESS_UPDATE:
			log.Printf("[executor] task %s progress: %.0f%% %s",
				progress.TaskId, progress.ProgressPercent, progress.Message)

		case pb.ProgressType_PROGRESS_LOG:
			log.Printf("[executor] task %s log: %s", progress.TaskId, progress.Message)
		}
	}
}
