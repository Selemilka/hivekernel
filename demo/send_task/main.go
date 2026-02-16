// Command send_task connects to a running HiveKernel and sends a task
// to the first spawned agent. Usage:
//
//	go run demo/send_task/main.go "What is 2+2?"
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	pb "github.com/selemilka/hivekernel/api/proto/hivepb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

func main() {
	addr := flag.String("addr", "localhost:50051", "HiveKernel CoreService address")
	timeout := flag.Duration("timeout", 60*time.Second, "task execution timeout")
	flag.Parse()

	description := "What is 2+2?"
	if flag.NArg() > 0 {
		description = flag.Arg(0)
	}

	// Connect to CoreService.
	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := pb.NewCoreServiceClient(conn)

	// Authenticate as PID 1 (kernel) -- demo client acts as admin.
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-hivekernel-pid", "1")

	// Find spawned agents via ListChildren.
	children, err := client.ListChildren(ctx, &pb.ListChildrenRequest{})
	if err != nil {
		log.Fatalf("ListChildren: %v", err)
	}
	if len(children.Children) == 0 {
		fmt.Fprintln(os.Stderr, "No agents found. Is HiveKernel running with a startup config?")
		os.Exit(1)
	}

	target := children.Children[0]
	fmt.Fprintf(os.Stderr, "Sending task to PID %d (%s, model=%s)\n", target.Pid, target.Name, target.Model)
	fmt.Fprintf(os.Stderr, "Task: %s\n", description)
	fmt.Fprintf(os.Stderr, "---\n")

	// Execute task.
	resp, err := client.ExecuteTask(ctx, &pb.ExecuteTaskRequest{
		TargetPid:   target.Pid,
		Description: description,
	})
	if err != nil {
		log.Fatalf("ExecuteTask: %v", err)
	}

	if !resp.Success {
		fmt.Fprintf(os.Stderr, "Task failed: %s\n", resp.Error)
		os.Exit(1)
	}

	fmt.Println(resp.Result.Output)
}
