package kernel

import (
	"context"
	"fmt"
	"testing"

	pb "github.com/selemilka/hivekernel/api/proto/hivepb"

	"google.golang.org/grpc/metadata"
)

// withPIDMetadata returns a context with the given PID in gRPC metadata.
func withPIDMetadata(ctx context.Context, pid uint64) context.Context {
	md := metadata.New(map[string]string{
		"x-hivekernel-pid": fmt.Sprintf("%d", pid),
	})
	return metadata.NewIncomingContext(ctx, md)
}

func TestCoreServer_ListSiblings(t *testing.T) {
	king := newTestKingForSyscall(t)
	cs := NewCoreServer(king)

	// Spawn two children under king.
	handler := NewKernelSyscallHandler(king, nil)
	r1 := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "spawn-a",
		Call: &pb.SystemCall_Spawn{
			Spawn: &pb.SpawnRequest{
				Name: "child-a", Role: pb.AgentRole_ROLE_WORKER,
				CognitiveTier: pb.CognitiveTier_COG_TACTICAL,
			},
		},
	})
	pidA := r1.GetSpawn().ChildPid

	r2 := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "spawn-b",
		Call: &pb.SystemCall_Spawn{
			Spawn: &pb.SpawnRequest{
				Name: "child-b", Role: pb.AgentRole_ROLE_WORKER,
				CognitiveTier: pb.CognitiveTier_COG_TACTICAL,
			},
		},
	})
	pidB := r2.GetSpawn().ChildPid

	// child-a asks for siblings -> should see child-b but not itself.
	ctx := withPIDMetadata(context.Background(), pidA)
	resp, err := cs.ListSiblings(ctx, &pb.ListSiblingsRequest{})
	if err != nil {
		t.Fatalf("ListSiblings: %v", err)
	}

	if len(resp.Siblings) != 1 {
		t.Fatalf("expected 1 sibling, got %d", len(resp.Siblings))
	}
	if resp.Siblings[0].Pid != pidB {
		t.Errorf("sibling pid = %d, want %d", resp.Siblings[0].Pid, pidB)
	}
	if resp.Siblings[0].Name != "child-b" {
		t.Errorf("sibling name = %q, want %q", resp.Siblings[0].Name, "child-b")
	}
}

func TestCoreServer_ListSiblings_NoSiblings(t *testing.T) {
	king := newTestKingForSyscall(t)
	cs := NewCoreServer(king)

	// Spawn one child.
	handler := NewKernelSyscallHandler(king, nil)
	r := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "spawn-only",
		Call: &pb.SystemCall_Spawn{
			Spawn: &pb.SpawnRequest{
				Name: "only-child", Role: pb.AgentRole_ROLE_WORKER,
				CognitiveTier: pb.CognitiveTier_COG_TACTICAL,
			},
		},
	})
	pid := r.GetSpawn().ChildPid

	ctx := withPIDMetadata(context.Background(), pid)
	resp, err := cs.ListSiblings(ctx, &pb.ListSiblingsRequest{})
	if err != nil {
		t.Fatalf("ListSiblings: %v", err)
	}
	if len(resp.Siblings) != 0 {
		t.Errorf("expected 0 siblings, got %d", len(resp.Siblings))
	}
}

func TestCoreServer_WaitChild_AlreadyZombie(t *testing.T) {
	king := newTestKingForSyscall(t)
	cs := NewCoreServer(king)

	// Spawn a child under king.
	handler := NewKernelSyscallHandler(king, nil)
	r := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "spawn-wait",
		Call: &pb.SystemCall_Spawn{
			Spawn: &pb.SpawnRequest{
				Name: "wait-target", Role: pb.AgentRole_ROLE_TASK,
				CognitiveTier: pb.CognitiveTier_COG_OPERATIONAL,
			},
		},
	})
	childPID := r.GetSpawn().ChildPid

	// Complete the task so it becomes zombie.
	_, err := king.Lifecycle().CompleteTask(childPID, 7, []byte("done"), "")
	if err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}

	ctx := withPIDMetadata(context.Background(), king.PID())
	resp, err := cs.WaitChild(ctx, &pb.WaitChildRequest{
		TargetPid:      childPID,
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("WaitChild: %v", err)
	}
	if !resp.Success {
		t.Fatalf("WaitChild failed: %s", resp.Error)
	}
	if resp.Pid != childPID {
		t.Errorf("pid = %d, want %d", resp.Pid, childPID)
	}
	if resp.ExitCode != 7 {
		t.Errorf("exit_code = %d, want 7", resp.ExitCode)
	}
	if resp.Output != "done" {
		t.Errorf("output = %q, want %q", resp.Output, "done")
	}
}

func TestCoreServer_WaitChild_NotOwn(t *testing.T) {
	king := newTestKingForSyscall(t)
	cs := NewCoreServer(king)

	handler := NewKernelSyscallHandler(king, nil)
	r := handler.HandleSyscall(context.Background(), king.PID(), &pb.SystemCall{
		CallId: "spawn-x",
		Call: &pb.SystemCall_Spawn{
			Spawn: &pb.SpawnRequest{
				Name: "child-x", Role: pb.AgentRole_ROLE_WORKER,
				CognitiveTier: pb.CognitiveTier_COG_TACTICAL,
			},
		},
	})
	childPID := r.GetSpawn().ChildPid

	// Fake caller PID 9999 tries to wait for king's child.
	ctx := withPIDMetadata(context.Background(), 9999)
	resp, err := cs.WaitChild(ctx, &pb.WaitChildRequest{
		TargetPid:      childPID,
		TimeoutSeconds: 1,
	})
	if err != nil {
		t.Fatalf("WaitChild: %v", err)
	}
	if resp.Success {
		t.Error("WaitChild should fail when caller is not parent")
	}
}

func TestCoreServer_WaitChild_NotFound(t *testing.T) {
	king := newTestKingForSyscall(t)
	cs := NewCoreServer(king)

	ctx := withPIDMetadata(context.Background(), king.PID())
	resp, err := cs.WaitChild(ctx, &pb.WaitChildRequest{
		TargetPid:      99999,
		TimeoutSeconds: 1,
	})
	if err != nil {
		t.Fatalf("WaitChild: %v", err)
	}
	if resp.Success {
		t.Error("WaitChild should fail for nonexistent process")
	}
}
