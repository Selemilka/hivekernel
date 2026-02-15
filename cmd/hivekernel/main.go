package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
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
	startupPath := flag.String("startup", "", "path to startup config JSON (empty = pure kernel, no agents)")
	sdkPath := flag.String("sdk-path", "", "path to Python SDK directory (auto-detected if empty)")
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

	// Load startup config (empty = no agents).
	startupCfg, err := kernel.LoadStartupConfig(*startupPath)
	if err != nil {
		log.Fatalf("Failed to load startup config: %v", err)
	}
	if len(startupCfg.Agents) > 0 {
		log.Printf("[startup] loaded %d agent(s) from %s", len(startupCfg.Agents), *startupPath)
	} else {
		log.Printf("[startup] pure kernel mode (no agents)")
	}

	// Normalize coreAddr for agents to dial back.
	coreAddr := normalizeCoreAddr(cfg.ListenAddr)

	// Resolve SDK path for Python agents.
	resolvedSDK := resolveSDKPath(*sdkPath)
	if resolvedSDK != "" {
		log.Printf("[startup] Python SDK path: %s", resolvedSDK)
	}

	// Start runtime manager, executor, and health monitor.
	rtManager := runtime.NewManager(coreAddr, "python")
	if resolvedSDK != "" {
		rtManager.SetSDKPath(resolvedSDK)
	}
	king.SetRuntimeManager(rtManager)
	syscallHandler := kernel.NewKernelSyscallHandler(king, rtManager)
	executor := runtime.NewExecutor(syscallHandler)
	syscallHandler.SetExecutor(executor)

	healthMon := runtime.NewHealthMonitor(
		rtManager,
		10*time.Second, // check interval
		3,              // max consecutive failures before kill
		5*time.Second,  // ping timeout
	)
	healthMon.OnUnhealthy(func(pid process.PID) {
		log.Printf("[health] PID %d unreachable, killing", pid)
		_ = rtManager.StopRuntime(pid)
		_ = king.Registry().SetState(pid, process.StateZombie)
		king.Signals().NotifyParent(pid, -1, "health: unreachable")
	})

	// Process exit watcher: instant death detection for auto-exit and crashes.
	rtManager.OnProcessExit(func(pid process.PID, exitCode int) {
		log.Printf("[runtime] PID %d exited (code %d), transitioning to zombie", pid, exitCode)
		_ = king.Registry().SetState(pid, process.StateZombie)
		king.Signals().NotifyParent(pid, exitCode, "process exited")
		healthMon.Remove(pid)
	})

	// Set up graceful shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// --- Start gRPC server ---
	grpcServer := grpc.NewServer()
	coreServer := kernel.NewCoreServer(king)
	coreServer.SetExecutor(executor)
	coreServer.Register(grpcServer)

	lis, err := listen(cfg.ListenAddr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", cfg.ListenAddr, err)
	}
	log.Printf("[grpc] CoreService listening on %s (agents dial %s)", cfg.ListenAddr, coreAddr)

	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			log.Printf("[grpc] server error: %v", err)
		}
	}()

	// Spawn agents from startup config.
	if len(startupCfg.Agents) > 0 {
		go func() {
			// Small delay to ensure gRPC server is fully ready.
			time.Sleep(200 * time.Millisecond)
			spawnStartupAgents(king, cfg, startupCfg)
		}()
	}

	// Start health monitor.
	go healthMon.Run(ctx)

	// Start cron poller (checks due entries every 30s).
	king.SetExecutor(executor)
	go king.RunCronPoller(ctx)

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

// normalizeCoreAddr converts a listen address to a dialable address.
func normalizeCoreAddr(listenAddr string) string {
	if strings.HasPrefix(listenAddr, "unix://") {
		return listenAddr
	}
	if strings.HasPrefix(listenAddr, ":") {
		return "localhost" + listenAddr
	}
	return listenAddr
}

// listen creates a net.Listener for TCP or unix socket addresses.
func listen(addr string) (net.Listener, error) {
	if strings.HasPrefix(addr, "unix://") {
		path := strings.TrimPrefix(addr, "unix://")
		os.Remove(path)
		return net.Listen("unix", path)
	}
	return net.Listen("tcp", addr)
}

// spawnStartupAgents spawns all agents defined in the startup config.
func spawnStartupAgents(king *kernel.King, cfg kernel.Config, startupCfg kernel.StartupConfig) {
	for _, agent := range startupCfg.Agents {
		name := agent.Name
		// Append node name to daemon names if not already present.
		if agent.Role == "daemon" && !strings.Contains(name, "@") {
			name = name + "@" + cfg.NodeName
		}

		proc, err := king.SpawnChild(process.SpawnRequest{
			ParentPID:     king.PID(),
			Name:          name,
			Role:          kernel.ParseRole(agent.Role),
			CognitiveTier: kernel.ParseTier(agent.CognitiveTier),
			Model:         agent.Model,
			User:          "root",
			Limits:        cfg.DefaultLimits,
			RuntimeType:   agent.RuntimeType,
			RuntimeImage:  agent.RuntimeImage,
			SystemPrompt:  agent.SystemPrompt,
		})
		if err != nil {
			log.Printf("[startup] failed to spawn %s: %v", agent.Name, err)
			continue
		}
		log.Printf("[startup] spawned %s (PID %d)", agent.Name, proc.PID)

		// Register cron entries for this agent.
		for _, cronEntry := range agent.Cron {
			if _, err := king.Cron().ParseAndAdd(cronEntry.Name, cronEntry.Expression, "execute", proc.PID, cronEntry.Description, cronEntry.Params); err != nil {
				log.Printf("[startup] failed to add cron %q for %s: %v", cronEntry.Name, agent.Name, err)
			} else {
				log.Printf("[startup] cron %q (%s) registered for %s (PID %d)", cronEntry.Name, cronEntry.Expression, agent.Name, proc.PID)
			}
		}
	}

	king.PrintProcessTable()
}

// resolveSDKPath finds the Python SDK directory.
// Priority: explicit flag > env var > relative to exe > relative to cwd.
func resolveSDKPath(explicit string) string {
	if explicit != "" {
		if info, err := os.Stat(explicit); err == nil && info.IsDir() {
			abs, _ := filepath.Abs(explicit)
			return abs
		}
		log.Printf("[startup] WARNING: --sdk-path %q not found", explicit)
		return ""
	}

	// Check env var.
	if envPath := os.Getenv("HIVEKERNEL_SDK_PATH"); envPath != "" {
		if info, err := os.Stat(envPath); err == nil && info.IsDir() {
			abs, _ := filepath.Abs(envPath)
			return abs
		}
	}

	// Check relative to executable.
	if exePath, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exePath)
		candidate := filepath.Join(exeDir, "..", "sdk", "python")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			abs, _ := filepath.Abs(candidate)
			return abs
		}
	}

	// Check relative to current working directory.
	if cwd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(cwd, "sdk", "python")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			abs, _ := filepath.Abs(candidate)
			return abs
		}
	}

	return ""
}
