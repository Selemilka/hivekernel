# Plan: Debug Logging + Web Dashboard Enhancements

## Context

HiveKernel uses bare `log.Printf` (111 calls, 14 files) with no log levels -- makes debugging hard.
PicoClaw has a proper `pkg/logger` but `pkg/hive/` ignores it, using raw `log.Printf`.
The dashboard has basics (tree, spawn, chat) but lacks log filtering, metrics, and IPC inspection.

**Goal**: Structured debug-level logging everywhere + dashboard features for observability.

---

## Phase 1: HiveKernel slog Package + Migration

### 1A. Create `internal/hklog/hklog.go`
- Wrap `log/slog` (stdlib, Go 1.21+, no deps)
- `Init(level, logFile)` -- sets up TextHandler (stderr) + optional JSONHandler (file)
- `For(component) *slog.Logger` -- returns logger with `"component"` attr
- `SetLevel(level)` -- dynamic level change via `slog.LevelVar`
- `multiHandler` -- fan-out to console + file handlers
- Fallback: if `Init()` not called, `For()` returns `slog.Default()`

### 1B. Create `internal/hklog/hklog_test.go`
- TestParseLevel, TestFor, TestSetLevel, TestMultiHandler

### 1C. Update `cmd/hivekernel/main.go`
- Add flags: `--log-level` (debug/info/warn/error, default: info), `--log-file` (optional)
- Replace `log.SetFlags(...)` with `hklog.Init(*logLevel, *logFile)`
- Migrate all 21 `log.Printf` calls to slog

### 1D. Migrate remaining 90 `log.Printf` calls (13 files)

Prefix-to-component mapping:

| File | Prefix | Component | Calls |
|------|--------|-----------|-------|
| `internal/kernel/king.go` | [king], [cron] | "king", "cron" | ~10 |
| `internal/kernel/grpc_core.go` | [grpc] | "grpc" | 15 |
| `internal/kernel/syscall_handler.go` | [syscall] | "syscall" | 9 |
| `internal/process/supervisor.go` | [supervisor] | "supervisor" | 7 |
| `internal/runtime/manager.go` | [runtime] | "runtime" | 7 |
| `internal/process/tree.go` | [tree] | "tree" | 4 |
| `internal/process/lifecycle.go` | [lifecycle] | "lifecycle" | 4 |
| `internal/process/signals.go` | [signal] | "signal" | 4 |
| `internal/runtime/executor.go` | [executor] | "executor" | 3 |
| `internal/ipc/broker.go` | [broker] | "broker" | 2 |
| `internal/ipc/events.go` | [events] | "events" | 1 |
| `internal/runtime/health.go` | [health] | "health" | 1 |

Pattern: `log.Printf("[tag] PID %d msg", pid)` -> `logger.Info("msg", "pid", pid)`
Fatal: `log.Fatalf(...)` -> `logger.Error(...); os.Exit(1)`

---

## Phase 2: Add Debug Logs to HiveKernel

All new logs are `logger.Debug(...)` -- invisible at default INFO level.

### IPC subsystem (currently very sparse)
- **broker.go**: routing decision, validation result, queue/inbox delivery, route type, sibling copy
- **queue.go** (0 logs): enqueue with priority/queue_len, skip expired, dequeue
- **priority.go** (0 logs): relationship determination, priority computation
- **shared_memory.go** (0 logs): store/retrieve/delete, access checks, visibility
- **pipe.go** (0 logs): create/close/write, buffer full
- **events.go** (1 log): subscribe/unsubscribe, publish with delivery count, dropped events

### Process subsystem
- **registry.go** (0 logs): register/remove/setState
- **spawner.go** (0 logs): validation steps, tier check, parent state check

### Runtime subsystem
- **health.go** (1 log): every ping attempt, failure count progression, recovery
- **manager.go**: READY wait, gRPC dial, Init RPC details

### Kernel
- **syscall_handler.go**: every syscall entry with full args, dispatch type, result summary

---

## Phase 3: PicoClaw Logger Unification

### Migrate `pkg/hive/` from `log.Printf` to `pkg/logger`

| File | Calls | Component |
|------|-------|-----------|
| server.go | 11 | "hive" |
| bridge.go | 2 | "hive-bridge" |
| direct_bridge.go | 10 | "hive-direct" |
| channel.go | 5 | "hive-channel" |

Pattern: `log.Printf("[hive] msg %v", x)` -> `logger.InfoC("hive", fmt.Sprintf("msg %v", x))`

### Add debug logs to `tools.go` (currently 0 logs)
- Each tool (HiveSpawnTool, HiveSendTool, HiveStoreTool, HiveWaitTool): log args on entry, result on exit
- Component: "hive-tools"

### Add debug logs to `bridge.go`
- callID assignment, pending channel, result received, close with pending count

---

## Phase 4: Web Dashboard Enhancements

### 4A. Kernel-side: Bridge slog to EventLog

Create `internal/hklog/eventlog_handler.go`:
- `LogEmitter` interface (avoids circular import with `process` package)
- `EventLogHandler` implements `slog.Handler`, emits INFO+ logs as `EventLogged` events
- Wire in `main.go`: after king creation, register emitter via `hklog.SetEventLogEmitter()`
- Dashboard already handles "log" events via WebSocket -- structured logs flow automatically

### 4B. Proto additions to `api/proto/core.proto`

New RPCs in CoreService:
```
rpc GetSystemMetrics(SystemMetricsRequest) returns (SystemMetrics);
rpc GetIPCState(IPCStateRequest) returns (IPCState);
```

New messages: SystemMetrics (total/active/zombie processes, tokens, uptime, per-process breakdown), IPCState (inboxes, pipes, shared_memory, subscriptions, recent_messages)

### 4C. New accessor methods for IPC inspection

- `broker.go`: `ListInboxes() map[PID]int` + recent message ring buffer (last 100)
- `pipe.go`: `PipeRegistry.List() []PipeInfo`
- `events.go`: `EventBus.ListTopics() map[string]int`

### 4D. gRPC handlers in `grpc_core.go`

- `GetSystemMetrics`: iterate registry, sum tokens, count states
- `GetIPCState`: call ListInboxes/List/ListTopics, format response

### 4E. Dashboard backend (`app.py`)

New endpoints:
- `GET /api/metrics` -> calls GetSystemMetrics
- `GET /api/ipc` -> calls GetIPCState

### 4F. Dashboard frontend

**Log Viewer** (enhanced log panel in index.html + app.js):
- Level filter buttons (ALL/DEBUG/INFO/WARN/ERROR) with color coding
- Component dropdown (auto-populated from received logs)
- Text search, auto-scroll with pause, clear
- Color: debug=gray, info=white, warn=yellow, error=red

**System Metrics** (new collapsible panel):
- Summary cards: total processes, active, total tokens, uptime
- D3.js horizontal bar chart: tokens per process
- Polls `/api/metrics` every 10s

**IPC Inspector** (new tabbed panel):
- Tabs: Queues | Recent Messages | Pipes | Shared Memory | Subscriptions
- Each tab renders a table from `/api/ipc`
- Recent Messages shows flow: `PID X -> PID Y [type] @ time`

---

## File Summary

### New files (4)
- `HiveKernel/internal/hklog/hklog.go` -- slog wrapper
- `HiveKernel/internal/hklog/hklog_test.go` -- tests
- `HiveKernel/internal/hklog/eventlog_handler.go` -- slog->EventLog bridge

### Modified files

**HiveKernel Go (20 files):**
- `cmd/hivekernel/main.go` -- flags, Init, 21 migrations, EventLogHandler wiring
- `internal/kernel/king.go` -- ~10 migrations + debug logs
- `internal/kernel/grpc_core.go` -- 15 migrations + 2 new RPC handlers
- `internal/kernel/syscall_handler.go` -- 9 migrations + debug logs
- `internal/process/supervisor.go` -- 7 migrations
- `internal/process/tree.go` -- 4 migrations + debug logs
- `internal/process/lifecycle.go` -- 4 migrations
- `internal/process/signals.go` -- 4 migrations
- `internal/process/registry.go` -- debug logs (new)
- `internal/process/spawner.go` -- debug logs (new)
- `internal/runtime/manager.go` -- 7 migrations + debug logs
- `internal/runtime/executor.go` -- 3 migrations + debug logs
- `internal/runtime/health.go` -- 1 migration + debug logs
- `internal/ipc/broker.go` -- 2 migrations + debug logs + ListInboxes() + recent ring buffer
- `internal/ipc/events.go` -- 1 migration + debug logs + ListTopics()
- `internal/ipc/queue.go` -- debug logs (new)
- `internal/ipc/shared_memory.go` -- debug logs (new)
- `internal/ipc/pipe.go` -- debug logs (new) + List()
- `internal/ipc/priority.go` -- debug logs (new)
- `api/proto/core.proto` -- 2 new RPCs + messages

**PicoClaw Go (5 files):**
- `pkg/hive/server.go` -- 11 migrations to pkg/logger
- `pkg/hive/bridge.go` -- 2 migrations + debug logs
- `pkg/hive/direct_bridge.go` -- 10 migrations
- `pkg/hive/channel.go` -- 5 migrations
- `pkg/hive/tools.go` -- debug logs (new)

**Dashboard (4 files):**
- `sdk/python/dashboard/app.py` -- /api/metrics, /api/ipc endpoints
- `sdk/python/dashboard/static/index.html` -- new panels
- `sdk/python/dashboard/static/app.js` -- log filter, metrics, IPC JS
- `sdk/python/dashboard/static/style.css` -- new styles

---

## Verification

1. **Build**: `go build -o bin/hivekernel.exe ./cmd/hivekernel` -- compiles
2. **Tests**: `go test ./internal/... -v` -- all 265 tests pass
3. **Default level**: `bin\hivekernel.exe` -- output similar to before (INFO only, text format)
4. **Debug level**: `bin\hivekernel.exe --log-level debug` -- verbose output with all debug logs
5. **JSON file**: `bin\hivekernel.exe --log-file kernel.log` -- structured JSON written to file
6. **PicoClaw build**: `cd picoclaw && go build -o bin/picoclaw.exe ./cmd/picoclaw`
7. **Dashboard**: `python sdk/python/dashboard/app.py` -- new panels render, logs filter correctly
8. **Proto regen**: after proto changes, regenerate Go + Python stubs

## Execution Order

Phase 1 -> Phase 2 -> Phase 4A-D (backend) -> Phase 4E-F (frontend)
Phase 3 runs independently (can be done in parallel with Phase 2)
