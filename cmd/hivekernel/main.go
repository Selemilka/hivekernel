package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	pb "github.com/selemilka/hivekernel/api/proto/hivepb"
	"github.com/selemilka/hivekernel/internal/hklog"
	"github.com/selemilka/hivekernel/internal/ipc"
	"github.com/selemilka/hivekernel/internal/kernel"
	"github.com/selemilka/hivekernel/internal/process"
	"github.com/selemilka/hivekernel/internal/runtime"
	"github.com/selemilka/hivekernel/internal/tracing"

	"google.golang.org/grpc"
)

func main() {
	nodeName := flag.String("node", "vps1", "VPS node name")
	listenAddr := flag.String("listen", ":50051", "gRPC listen address (host:port or unix:///path)")
	startupPath := flag.String("startup", "", "path to startup config JSON (empty = pure kernel, no agents)")
	sdkPath := flag.String("sdk-path", "", "path to Python SDK directory (auto-detected if empty)")
	clawBin := flag.String("claw-bin", "", "path to PicoClaw binary (for RUNTIME_CLAW agents)")
	envFile := flag.String("env-file", ".env", "path to .env file (empty to skip)")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	logFile := flag.String("log-file", "", "path to JSON log file (optional)")
	flag.Parse()

	// Set session timestamp for dialog logging -- inherited by all child processes.
	os.Setenv("HIVE_SESSION_TS", time.Now().Format("20060102-150405"))

	hklog.Init(*logLevel, *logFile)
	logger := hklog.For("main")

	// Initialize OpenTelemetry gRPC tracing (JSONL export to logs/otel/).
	traceShutdown, err := tracing.Setup(os.Getenv("HIVE_SESSION_TS"), *nodeName)
	if err != nil {
		logger.Warn("tracing setup failed", "error", err)
	}

	// Load .env file if it exists.
	if *envFile != "" {
		if n, err := loadDotEnv(*envFile); err != nil {
			logger.Warn("could not load env file", "file", *envFile, "error", err)
		} else if n > 0 {
			logger.Info("loaded env vars", "count", n, "file", *envFile)
		}
	}

	cfg := kernel.DefaultConfig()
	cfg.NodeName = *nodeName
	cfg.ListenAddr = *listenAddr

	logger.Info("HiveKernel starting", "node", cfg.NodeName)

	// Bootstrap the kernel.
	king, err := kernel.New(cfg)
	if err != nil {
		logger.Error("failed to bootstrap kernel", "error", err)
		os.Exit(1)
	}

	// Wire PID resolver for otel span annotations.
	tracing.SetPIDResolver(king)

	// Bridge slog INFO+ to EventLog so kernel logs appear in dashboard.
	hklog.SetEventLogEmitter(king)
	hklog.AddEventLogHandler()

	// Load startup config (empty = no agents).
	startupCfg, err := kernel.LoadStartupConfig(*startupPath)
	if err != nil {
		logger.Error("failed to load startup config", "error", err)
		os.Exit(1)
	}
	if len(startupCfg.Agents) > 0 {
		logger.Info("loaded startup config", "agents", len(startupCfg.Agents), "path", *startupPath)
	} else {
		logger.Info("pure kernel mode, no agents")
	}

	// Normalize coreAddr for agents to dial back.
	coreAddr := normalizeCoreAddr(cfg.ListenAddr)

	// Resolve SDK path for Python agents.
	resolvedSDK := resolveSDKPath(*sdkPath)
	if resolvedSDK != "" {
		logger.Info("Python SDK path resolved", "path", resolvedSDK)
	}

	// Start runtime manager, executor, and health monitor.
	rtManager := runtime.NewManager(coreAddr, "python")
	if resolvedSDK != "" {
		rtManager.SetSDKPath(resolvedSDK)
	}
	if *clawBin != "" {
		rtManager.SetClawBin(*clawBin)
		logger.Info("PicoClaw binary set", "path", *clawBin)
	}
	king.SetRuntimeManager(rtManager)

	// Wire push delivery: when a message arrives in a process's inbox,
	// immediately deliver it to the agent's gRPC DeliverMessage RPC.
	brokerLog := hklog.For("broker")
	king.Broker().OnMessage = func(pid process.PID, msg *ipc.Message) {
		client := rtManager.GetClient(pid)
		if client == nil {
			brokerLog.Warn("push delivery skipped, no runtime client", "pid", pid, "type", msg.Type)
			return
		}
		pbMsg := &pb.AgentMessage{
			MessageId:   msg.ID,
			FromPid:     msg.FromPID,
			FromName:    msg.FromName,
			Type:        msg.Type,
			Priority:    pb.Priority(msg.Priority),
			Payload:     msg.Payload,
			RequiresAck: msg.RequiresAck,
			ReplyTo:     msg.ReplyTo,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := client.DeliverMessage(ctx, pbMsg); err != nil {
			brokerLog.Error(fmt.Sprintf("push delivery failed to PID %d: %v", pid, err), "type", msg.Type, "from_pid", msg.FromPID)
			return
		}
		// Emit message_delivered event.
		if el := king.EventLog(); el != nil {
			el.Emit(process.ProcessEvent{
				Type:      process.EventMessageDelivered,
				PID:       pid,
				PPID:      msg.FromPID,
				MessageID: msg.ID,
			})
		}
	}

	syscallHandler := kernel.NewKernelSyscallHandler(king, rtManager)
	executor := runtime.NewExecutor(syscallHandler)
	syscallHandler.SetExecutor(executor)

	healthMon := runtime.NewHealthMonitor(
		rtManager,
		10*time.Second, // check interval
		3,              // max consecutive failures before kill
		5*time.Second,  // ping timeout
	)
	healthLog := hklog.For("health")
	healthMon.OnUnhealthy(func(pid process.PID) {
		healthLog.Warn("agent unreachable, killing", "pid", pid)
		_ = rtManager.StopRuntime(pid)
		_ = king.Registry().SetState(pid, process.StateZombie)
		king.Signals().NotifyParent(pid, -1, "health: unreachable")
	})

	// Start supervisor (zombie reaping, restart policies).
	supervisor := process.NewSupervisor(
		king.Registry(),
		king.Signals(),
		process.NewTreeOps(king.Registry(), king.Signals()),
		process.DefaultSupervisorConfig(),
	)

	// Wire restart callback: supervisor calls this when a daemon needs restarting.
	supervisor.OnRestart(func(proc *process.Process) error {
		// Reset RuntimeAddr to image spec for respawn (StartRuntime reads it as image).
		proc.RuntimeAddr = proc.RuntimeImage
		rtType := runtime.RuntimeType(proc.RuntimeType)
		if rtType == "" {
			rtType = runtime.RuntimePython
		}
		_, err := rtManager.StartRuntime(proc, rtType)
		return err
	})

	// Process exit watcher: instant death detection for auto-exit and crashes.
	// Delegates to supervisor which decides: restart (daemons) or zombie+notify (agents/tasks).
	rtLog := hklog.For("runtime")
	rtManager.OnProcessExit(func(pid process.PID, exitCode int) {
		rtLog.Info("process exited", "pid", pid, "exit_code", exitCode)
		healthMon.Remove(pid)
		supervisor.HandleChildExit(pid, exitCode)
	})

	// Set up graceful shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// --- Start gRPC server ---
	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(tracing.ServerUnaryInterceptor()),
		grpc.ChainStreamInterceptor(tracing.ServerStreamInterceptor()),
	)
	coreServer := kernel.NewCoreServer(king)
	coreServer.SetExecutor(executor)
	coreServer.Register(grpcServer)

	lis, err := listen(cfg.ListenAddr)
	if err != nil {
		logger.Error("failed to listen", "addr", cfg.ListenAddr, "error", err)
		os.Exit(1)
	}
	logger.Info("CoreService listening", "addr", cfg.ListenAddr, "dial_addr", coreAddr)

	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			slog.Error("gRPC server error", "error", err)
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

	go supervisor.Run(ctx)

	// Start health monitor.
	go healthMon.Run(ctx)

	// Start cron poller (checks due entries every 30s).
	king.SetExecutor(executor)
	go king.RunCronPoller(ctx)

	// Start kernel main loop.
	go func() {
		if err := king.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("kernel error", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for shutdown signal.
	sig := <-sigCh
	logger.Info("received shutdown signal", "signal", sig)

	// 1. Cancel context — signals all goroutines (cron poller, health monitor, king.Run).
	cancel()

	// 2. Close event log — unblocks SubscribeEvents streams by closing subscriber channels.
	if el := king.EventLog(); el != nil {
		el.Close()
	}

	// 3. Graceful shutdown in background: stop agents, then gRPC server.
	done := make(chan struct{})
	go func() {
		rtManager.StopAll()
		grpcServer.GracefulStop()
		close(done)
	}()

	// 4. Wait for graceful completion, second signal, or total timeout.
	select {
	case <-done:
		// Clean exit.
	case <-sigCh:
		logger.Warn("force shutdown")
		rtManager.KillAll()
		grpcServer.Stop()
	case <-time.After(15 * time.Second):
		logger.Warn("graceful shutdown timed out, forcing stop")
		rtManager.KillAll()
		grpcServer.Stop()
	}

	// Flush otel traces.
	if traceShutdown != nil {
		flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
		traceShutdown(flushCtx)
		flushCancel()
	}

	king.Stop()
	logger.Info("HiveKernel stopped")
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
	startupLog := hklog.For("startup")
	for _, agent := range startupCfg.Agents {
		name := agent.Name
		// Append node name to daemon names if not already present.
		if agent.Role == "daemon" && !strings.Contains(name, "@") {
			name = name + "@" + cfg.NodeName
		}

		// Build metadata map: start with explicit metadata, then ClawConfig, then tools/workspace.
		metadata := make(map[string]string)
		for k, v := range agent.Metadata {
			metadata[k] = v
		}
		clawMeta := kernel.ClawConfigToMetadata(agent.ClawConfig)
		for k, v := range clawMeta {
			metadata[k] = v
		}
		if len(agent.Tools) > 0 {
			metadata["agent.tools"] = strings.Join(agent.Tools, ",")
		}
		if agent.Workspace != "" {
			metadata["agent.workspace"] = agent.Workspace
		}

		proc, err := king.SpawnChild(process.SpawnRequest{
			ParentPID:     king.PID(),
			Name:          name,
			Role:          kernel.ParseRole(agent.Role),
			CognitiveTier: kernel.ParseTier(agent.CognitiveTier),
			Model:         agent.Model,
			User:          "root",
			Limits:        cfg.DefaultLimits,
			RuntimeType:   kernel.ParseRuntimeType(agent.RuntimeType),
			RuntimeImage:  agent.RuntimeImage,
			SystemPrompt:  agent.SystemPrompt,
			Metadata:      metadata,
		})
		if err != nil {
			startupLog.Error("failed to spawn agent", "name", agent.Name, "error", err)
			continue
		}
		startupLog.Info("spawned agent", "name", agent.Name, "pid", proc.PID)

		// Flush any messages that arrived before the agent was ready.
		if flushed := king.Broker().FlushInbox(proc.PID); flushed > 0 {
			startupLog.Info("flushed pending messages", "count", flushed, "name", agent.Name, "pid", proc.PID)
		}

		// Register cron entries for this agent.
		for _, cronEntry := range agent.Cron {
			action := cronEntry.Action
			if action == "" {
				action = "message"
			}
			if _, err := king.Cron().ParseAndAdd(cronEntry.Name, cronEntry.Expression, action, proc.PID, cronEntry.Description, cronEntry.Params); err != nil {
				startupLog.Error("failed to add cron", "cron", cronEntry.Name, "agent", agent.Name, "error", err)
			} else {
				startupLog.Info("cron registered", "cron", cronEntry.Name, "expr", cronEntry.Expression, "action", action, "agent", agent.Name, "pid", proc.PID)
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
		slog.Warn("sdk-path not found", "path", explicit)
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

// loadDotEnv reads a .env file and sets variables into os environment.
// Lines must be KEY=VALUE (quotes around value are stripped).
// Lines starting with # and empty lines are skipped.
// Only sets vars that are not already set in the environment.
// Returns the number of variables loaded.
func loadDotEnv(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // missing .env is not an error
		}
		return 0, err
	}
	defer f.Close()

	n := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// Strip surrounding quotes.
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		// Only set if not already present.
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
			n++
		}
	}
	return n, scanner.Err()
}
