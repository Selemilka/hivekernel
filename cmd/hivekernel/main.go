package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/selemilka/hivekernel/internal/kernel"
	"github.com/selemilka/hivekernel/internal/process"
	"github.com/selemilka/hivekernel/internal/runtime"

	"google.golang.org/grpc"
)

func main() {
	nodeName := flag.String("node", "vps1", "VPS node name")
	listenAddr := flag.String("listen", ":50051", "gRPC listen address (host:port or unix:///path)")
	flag.Parse()

	cfg := kernel.DefaultConfig()
	cfg.NodeName = *nodeName
	cfg.ListenAddr = *listenAddr

	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("HiveKernel starting on node %s", cfg.NodeName)

	// Bootstrap the kernel.
	king, err := kernel.New(cfg)
	if err != nil {
		log.Fatalf("Failed to bootstrap kernel: %v", err)
	}

	// Start runtime manager, executor, and health monitor.
	rtManager := runtime.NewManager()
	syscallHandler := kernel.NewKernelSyscallHandler(king)
	executor := runtime.NewExecutor(syscallHandler)
	_ = executor // used when core dispatches tasks to agents

	healthMon := runtime.NewHealthMonitor(
		king.Registry(),
		rtManager,
		10*time.Second, // check interval
		30*time.Second, // timeout
	)
	healthMon.OnUnhealthy(func(pid process.PID) {
		log.Printf("[main] unhealthy agent PID %d detected", pid)
	})

	// Set up graceful shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// --- Start gRPC server ---
	grpcServer := grpc.NewServer()
	coreServer := kernel.NewCoreServer(king)
	coreServer.Register(grpcServer)

	lis, err := listen(cfg.ListenAddr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", cfg.ListenAddr, err)
	}
	log.Printf("[grpc] CoreService listening on %s", cfg.ListenAddr)

	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			log.Printf("[grpc] server error: %v", err)
		}
	}()

	// Phase 0 demo: spawn queen + worker to verify the pipeline.
	go func() {
		if err := demo(king, rtManager, cfg); err != nil {
			log.Printf("[demo] error: %v", err)
		}
	}()

	// Start health monitor.
	go healthMon.Run(ctx)

	// Start kernel main loop.
	go func() {
		if err := king.Run(ctx); err != nil && ctx.Err() == nil {
			log.Fatalf("Kernel error: %v", err)
		}
	}()

	// Wait for shutdown signal.
	sig := <-sigCh
	log.Printf("Received %s, shutting down...", sig)
	grpcServer.GracefulStop()
	king.Stop()
	cancel()
	log.Printf("HiveKernel stopped.")
}

// listen creates a net.Listener for TCP or unix socket addresses.
func listen(addr string) (net.Listener, error) {
	if strings.HasPrefix(addr, "unix://") {
		path := strings.TrimPrefix(addr, "unix://")
		// Remove stale socket file.
		os.Remove(path)
		return net.Listen("unix", path)
	}
	return net.Listen("tcp", addr)
}

// demo spawns the Phase 0 scenario: king -> queen -> worker.
func demo(king *kernel.King, rtManager *runtime.Manager, cfg kernel.Config) error {
	// Spawn queen@vps1 under king.
	queen, err := king.SpawnChild(process.SpawnRequest{
		ParentPID:     king.PID(),
		Name:          "queen@vps1",
		Role:          process.RoleDaemon,
		CognitiveTier: process.CogTactical,
		Model:         "sonnet",
		User:          "root",
		Limits:        cfg.DefaultLimits,
	})
	if err != nil {
		return err
	}

	// Register runtime for queen.
	_, err = rtManager.StartRuntime(queen, runtime.RuntimePython)
	if err != nil {
		return err
	}

	// Spawn a worker under queen.
	worker, err := king.SpawnChild(process.SpawnRequest{
		ParentPID:     queen.PID,
		Name:          "demo-worker",
		Role:          process.RoleWorker,
		CognitiveTier: process.CogTactical,
		Model:         "sonnet",
		Limits:        cfg.DefaultLimits,
	})
	if err != nil {
		return err
	}

	_, err = rtManager.StartRuntime(worker, runtime.RuntimePython)
	if err != nil {
		return err
	}

	// Print the process table.
	king.PrintProcessTable()

	log.Printf("[demo] Phase 0 scenario ready: king(PID %d) → queen(PID %d) → worker(PID %d)",
		king.PID(), queen.PID, worker.PID)

	return nil
}
