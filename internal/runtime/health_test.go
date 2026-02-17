package runtime

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/selemilka/hivekernel/api/proto/hivepb"
	"github.com/selemilka/hivekernel/internal/process"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// healthyAgent responds to Heartbeat with STATE_RUNNING.
type healthyAgent struct {
	pb.UnimplementedAgentServiceServer
	pingCount atomic.Int32
}

func (a *healthyAgent) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	a.pingCount.Add(1)
	return &pb.HeartbeatResponse{State: pb.AgentState_STATE_RUNNING}, nil
}

func startHealthAgent(t *testing.T, agent *healthyAgent) (string, *grpc.ClientConn) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	pb.RegisterAgentServiceServer(srv, agent)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.GracefulStop)

	addr := lis.Addr().String()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return addr, conn
}

func makeManager(runtimes map[process.PID]*AgentRuntime) *Manager {
	m := &Manager{
		runtimes: runtimes,
	}
	return m
}

func TestHealthMonitor_HealthyAgent(t *testing.T) {
	agent := &healthyAgent{}
	addr, conn := startHealthAgent(t, agent)
	client := pb.NewAgentServiceClient(conn)

	mgr := makeManager(map[process.PID]*AgentRuntime{
		10: {PID: 10, Addr: addr, Client: client},
	})

	var unhealthyCalled atomic.Int32
	hm := NewHealthMonitor(mgr, 50*time.Millisecond, 3, 2*time.Second)
	hm.OnUnhealthy(func(pid process.PID) {
		unhealthyCalled.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	go hm.Run(ctx)

	// Let a few health checks pass.
	time.Sleep(200 * time.Millisecond)
	cancel()

	if agent.pingCount.Load() == 0 {
		t.Error("expected at least one ping, got 0")
	}
	if unhealthyCalled.Load() != 0 {
		t.Errorf("expected 0 unhealthy calls, got %d", unhealthyCalled.Load())
	}
}

func TestHealthMonitor_UnreachableAgent(t *testing.T) {
	// Create a client to a closed port — pings will fail.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close() // immediately close — agent is "dead"

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	client := pb.NewAgentServiceClient(conn)

	mgr := makeManager(map[process.PID]*AgentRuntime{
		20: {PID: 20, Addr: addr, Client: client},
	})

	var unhealthyPID atomic.Uint64
	hm := NewHealthMonitor(mgr, 50*time.Millisecond, 3, 500*time.Millisecond)
	hm.OnUnhealthy(func(pid process.PID) {
		unhealthyPID.Store(pid)
	})

	ctx, cancel := context.WithCancel(context.Background())
	go hm.Run(ctx)

	// Wait for 3+ failed pings (50ms interval * ~4 ticks).
	time.Sleep(400 * time.Millisecond)
	cancel()

	if unhealthyPID.Load() != 20 {
		t.Errorf("expected unhealthy callback for PID 20, got PID %d", unhealthyPID.Load())
	}
}

func TestHealthMonitor_NilClient(t *testing.T) {
	// Runtime with nil Client (virtual process) — should be skipped, not marked unhealthy.
	mgr := makeManager(map[process.PID]*AgentRuntime{
		30: {PID: 30, Client: nil},
	})

	var unhealthyCalled atomic.Int32
	hm := NewHealthMonitor(mgr, 50*time.Millisecond, 2, 500*time.Millisecond)
	hm.OnUnhealthy(func(pid process.PID) {
		unhealthyCalled.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	go hm.Run(ctx)

	time.Sleep(200 * time.Millisecond)
	cancel()

	if unhealthyCalled.Load() != 0 {
		t.Errorf("expected 0 unhealthy calls for virtual process, got %d", unhealthyCalled.Load())
	}
}

func TestHealthMonitor_Remove(t *testing.T) {
	hm := NewHealthMonitor(makeManager(nil), time.Minute, 3, time.Second)
	hm.failures[process.PID(5)] = 2
	hm.Remove(5)

	if _, ok := hm.failures[5]; ok {
		t.Error("expected failure count to be removed")
	}
}

func TestHealthMonitor_FailureCounterResets(t *testing.T) {
	agent := &healthyAgent{}
	addr, conn := startHealthAgent(t, agent)
	client := pb.NewAgentServiceClient(conn)

	mgr := makeManager(map[process.PID]*AgentRuntime{
		40: {PID: 40, Addr: addr, Client: client},
	})

	hm := NewHealthMonitor(mgr, time.Minute, 3, 2*time.Second)

	// Manually set some failures.
	hm.failures[process.PID(40)] = 2

	// Run one check — agent is healthy, counter should reset.
	hm.check(context.Background())

	hm.mu.Lock()
	count := hm.failures[process.PID(40)]
	hm.mu.Unlock()

	if count != 0 {
		t.Errorf("expected failure count 0 after successful ping, got %d", count)
	}
}
