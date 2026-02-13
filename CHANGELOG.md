# HiveKernel Changelog

## Phase 0 — Proof of Concept (completed)

**Goal:** king -> queen -> worker, verify IPC works end-to-end.

### Go Core
- `api/proto/agent.proto` — AgentService + all shared enums/messages
- `api/proto/core.proto` — CoreService definition
- `api/proto/hivepb/` — generated Go gRPC code
- `internal/process/types.go` — Process, AgentRole, CognitiveTier, ProcessState
- `internal/process/registry.go` — thread-safe process table, tree traversal, NCA
- `internal/process/spawner.go` — spawn validation (cognitive tier, max_children, role compat)
- `internal/ipc/queue.go` — priority queue with aging (container/heap), TTL, blocking pop
- `internal/kernel/king.go` — PID 1 bootstrap, message loop
- `internal/kernel/config.go` — system configuration with defaults
- `internal/kernel/grpc_core.go` — CoreService gRPC server (SpawnChild, KillChild, GetProcessInfo, ListChildren, SendMessage, Escalate, Log, GetResourceUsage)
- `internal/kernel/convert.go` — proto <-> internal type converters
- `internal/runtime/manager.go` — agent runtime lifecycle (stub)
- `internal/runtime/health.go` — heartbeat monitoring, zombie detection
- `cmd/hivekernel/main.go` — entry point, gRPC server, demo scenario

### Python SDK
- `sdk/python/hivekernel_sdk/types.py` — TaskResult, Task, Message, AgentConfig
- `sdk/python/hivekernel_sdk/client.py` — CoreClient (gRPC wrapper)
- `sdk/python/hivekernel_sdk/agent.py` — HiveAgent base class (Init, Execute, Shutdown, Heartbeat, DeliverMessage)
- `sdk/python/examples/echo_worker.py` — demo agent
- `sdk/python/examples/test_e2e.py` — end-to-end integration test
- `sdk/python/pyproject.toml` — UV-managed project

### Tests: 15 passing
- Registry: CRUD, parent/child, NCA, PID auto-increment
- Spawner: kernel bootstrap, child spawn, cog tier violation, max_children, strategic task rejection, name required
- IPC queue: basic ordering, aging, TTL, blocking pop, cancel, ID generation

---

## Phase 1 — Process Tree + Supervision (completed)

**Goal:** crash detection, restart policies, zombie cleanup, tree operations.

### Added
- `internal/process/signals.go` — Signal types (SIGTERM, SIGKILL, SIGCHLD, SIGSTOP, SIGCONT, SIGHUP), signal delivery, grace period handling, parent notification
- `internal/process/tree.go` — KillBranch (bottom-up ordering), Reparent, OrphanAdoption, SubtreeVPS
- `internal/process/supervisor.go` — RestartPolicy per role (always/notify/never), exponential backoff, max restart cap, zombie reaping loop
- `internal/daemons/maid.go` — per-VPS health monitoring daemon (memory, zombies, process status)

### Tests: 27 passing (+12 new)
- Signals: SIGTERM->Blocked, SIGKILL->Dead, SIGCHLD delivery, grace period (TERM then KILL), STOP/CONT
- Tree: KillBranch (bottom-up), Reparent (+ SIGHUP), OrphanAdoption
- Supervisor: daemon auto-restart, task no-restart + parent notify, zombie reaping, max restart exceeded

---

## Phase 2 — Multi-agent + IPC (completed)

**Goal:** full IPC routing with priority aging, shared memory, broadcast events.

### Added
- `internal/ipc/broker.go` — Central message router with routing rule validation (parent<->child direct, siblings through broker with parent copy, cross-branch through NCA, tasks can only send to parent)
- `internal/ipc/priority.go` — Relationship-based priority (kernel > parent > sibling > child), combined with explicit priority
- `internal/ipc/shared_memory.go` — Artifact storage with 4 visibility levels (private, user, subtree, global), permission checks based on process tree and USER identity
- `internal/ipc/pipe.go` — Bidirectional byte channels parent<->child with backpressure, PipeRegistry for lifecycle management
- `internal/ipc/events.go` — Pub/sub broadcast EventBus with topic subscription, non-blocking delivery

### Changed
- `internal/kernel/king.go` — Now owns Broker, SharedMemory, EventBus, PipeRegistry; routes messages through broker
- `internal/kernel/grpc_core.go` — SendMessage uses broker (validates routing rules), Subscribe streams from broker queues, StoreArtifact/GetArtifact/ListArtifacts fully implemented

### Tests: 52 passing (+25 new)
- Broker: parent->child, child->parent, siblings (parent sees copy), task restricted to parent, named queues, cross-branch via NCA
- Shared memory: global/private/user/subtree visibility, list with prefix, owner delete, kernel delete, key required
- Pipes: bidirectional read/write, close, registry create/get/remove, backpressure
- Events: pub/sub, multiple subscribers, unsubscribe, no-subscriber safety, topic isolation

---

## Phase 3 — Resources + Permissions (completed)

**Goal:** token budgets, rate limiting, usage accounting, identity management, ACL, role capabilities.

### Added
- `internal/resources/budget.go` — Token budget management per process/model tier. Parent-to-child allocation, consumption tracking, release on death (unused returns to parent), branch usage aggregation
- `internal/resources/limits.go` — RateLimiter (sliding window per-process API call rate limiting), LimitChecker (spawn limits, context window, timeout enforcement)
- `internal/resources/accounting.go` — Usage tracking aggregated by user, VPS, PID, and model tier. Records token consumption events with timestamps
- `internal/permissions/auth.go` — USER identity resolution and inheritance validation. Kernel can assign any user, others must inherit from parent
- `internal/permissions/acl.go` — Access control lists with role-based default rules. Actions: spawn, kill, send_message, read/write_artifact, escalate, read_process. Cross-user access restricted (kernel only)
- `internal/permissions/capabilities.go` — Role-based capability system. Per-role capability sets (kernel=all, task=minimal). Per-process grant/revoke overrides. Tool validation (tasks cannot have shell_exec)

### Changed
- `internal/kernel/king.go` — Now owns BudgetManager, RateLimiter, LimitChecker, Accountant, AuthProvider, ACL, CapabilityChecker. SpawnChild validates ACL, capabilities, user inheritance, and budget before spawning. Kernel gets initial budget on bootstrap
- `internal/kernel/grpc_core.go` — SendMessage checks ACL and rate limits. StoreArtifact checks write permission. ReportMetric records token usage in accounting and budget. GetResourceUsage returns budget-aware remaining tokens. RequestResources fully implemented (allocates from parent budget)

### Tests: 87 passing (+35 new)
- Budget: set/get, allocate to child, insufficient funds, consume, exceed, release (return to parent), branch usage, tier mapping
- Limits: rate limiter allow/deny/expiry/remove/no-limit, spawn limit check, context window, timeout
- Accounting: record, user usage, VPS usage, process usage, total usage
- Auth: resolve identity, kernel assigns any user, non-kernel must inherit, same user check, is-kernel
- ACL: kernel can do anything, daemon permissions, worker spawn, task cannot spawn, task can send/read, cross-user denied/same-user allowed/kernel allowed, custom rules
- Capabilities: kernel has all, task no spawn/shell, worker can spawn, lead can kill, require, grant override, revoke override, validate tools, list capabilities

---

## Phase 4 — Multi-VPS (completed)

**Goal:** cluster node management, cross-VPS connections, branch migration, group resource limits.

### Added
- `internal/cluster/discovery.go` — NodeRegistry for VPS node tracking. NodeInfo (ID, address, capacity, health metrics). NodeStatus (online, overloaded, draining, offline). Register, Deregister, Get, List, ListOnline, UpdateHealth (auto-overload at 90% memory), SetStatus, Heartbeat, FindLeastLoaded (with exclusion), CheckStale
- `internal/cluster/connector.go` — VPS Connector for managing gRPC connections between nodes. Connect, Disconnect, GetConnection, IsConnected, ForwardMessage (with message logging). Error tracking with threshold-based unhealthy detection, RecordSuccess recovery. Connection pool lifecycle
- `internal/cluster/migration.go` — MigrationManager for process/branch migration between VPS. PrepareMigration (validate, snapshot branch), ExecuteMigration (update VPS in registry, update node counts), RollbackMigration. MigrationState tracking (pending, in_progress, completed, failed, rolled_back). ProcessSnapshot for state serialization. Guards: cannot migrate kernel, cannot migrate to same node, target must be online
- `internal/resources/cgroups.go` — CGroupManager for group resource limits by tree branch. Create (auto-includes root + descendants), Delete, Get, GetByPID, AddProcess, RemoveProcess. ConsumeTokens (collective budget enforcement), CheckSpawnAllowed (max processes per group). GroupUsage aggregation

### Changed
- `internal/kernel/king.go` — Now owns NodeRegistry, Connector, MigrationManager, CGroupManager. Registers self as cluster node on bootstrap. SpawnChild checks cgroup spawn limits and auto-adds children to parent's cgroup. Escalation handler recognizes "vps_overloaded" and triggers automatic migration to least-loaded node

### Tests: 154 passing (+57 new)
- Discovery: register/get, deregister, list, list online, update health, auto-overload detection, recovery, FindLeastLoaded (basic, exclude, skip offline, none available), CheckStale, MemoryFreePercent, LoadScore, Heartbeat, SetStatus
- Connector: connect/get, validation, disconnect, IsConnected, list, ForwardMessage (success, not connected, unhealthy), error threshold, recovery, error reset on forward, reconnect
- Migration: prepare (basic, cannot migrate kernel, same node, target offline), execute (basic, node count updates), rollback (basic, invalid state), execute twice, list/active, source draining restored, SnapshotProcess
- Cgroups: create (basic, duplicate, invalid root), delete, GetByPID, AddProcess (basic, max reached, idempotent), RemoveProcess, ConsumeTokens (basic, exceeds limit, not in group), CheckSpawnAllowed (basic, not in group), List, GetGroupUsage, Remaining, UsagePercent

---

## Phase 5 — Dynamic Scaling (completed)

**Goal:** task scheduling with priority aging, cron-based periodic actions, process lifecycle orchestration (sleep/wake, branch collapse), full compiler scenario.

### Added
- `internal/scheduler/priority.go` — TaskPriority with aging (`effective = base + age * factor`). BasePriorityFromTier/Role scoring. ReadyQueue (heap-based priority queue). TaskEntry with ID, Name, RequiredTier, RequiredRole, State. TaskState enum (Pending, Assigned, Running, Completed, Failed, Cancelled)
- `internal/scheduler/scheduler.go` — Task Scheduler with Submit, Assign (tier/role compatibility matching), Complete, Cancel, Resubmit. PendingCount, TasksByState, TasksByAgent queries. Auto-priority from tier+role on submit
- `internal/scheduler/cron.go` — CronSchedule parsing (supports `*`, specific values, `*/N` intervals, comma-separated). CronScheduler: Add, Remove, SetEnabled, CheckDue (with last-trigger dedup), ListByVPS. CronEntry with spawn/wake actions, VPS targeting, KeepAlive flag
- `internal/process/lifecycle.go` — LifecycleManager for dynamic process lifecycle. Sleep/Wake (architect pattern: produce artifact then sleep, wake on demand). CompleteTask (records result, marks dead, SIGCHLD to parent — like waitpid). WaitResult/PeekResult for result collection. CollapseBranch (kills all children bottom-up, collects results). ActiveChildren, IsAlive helpers

### Changed
- `internal/kernel/king.go` — Now owns Scheduler, CronScheduler, LifecycleManager. Creates SignalRouter for lifecycle management. Added accessor methods: Scheduler(), Cron(), Lifecycle()

### Tests: 210 passing (+56 new)
- Task priority: tier scoring (strategic > tactical > operational), role scoring (kernel > daemon > ... > task), combined priority, aging effect, ReadyQueue push/pop/peek/drain, aging reorder
- Scheduler: submit, assign (basic, tier mismatch, role mismatch, empty queue), complete (success, failure), cancel, resubmit, priority ordering, TasksByState, TasksByAgent
- Cron: parse (basic, interval, multi-value, invalid), matches (time, interval, day-of-week), add/remove, enable/disable, CheckDue (trigger, no re-trigger), ListByVPS
- Lifecycle: sleep (basic, idempotent, dead), wake (basic, not-sleeping), CompleteTask (success, failure), WaitResult (consume semantics), PeekResult, PendingResults, CollapseBranch (basic, with completed children, empty), IsAlive, ActiveChildren
- **Compiler scenario**: full Leo pipeline

---

## Phase 6 — Async Python SDK + In-Stream Syscalls (completed)

**Goal:** Migrate Python SDK from blocking `grpc` to `grpc.aio`, implement in-stream syscalls so agents can spawn/send/kill through the Execute bidirectional stream.

### Python SDK — Async Migration

- `sdk/python/hivekernel_sdk/client.py` — All 13 CoreClient methods now `async def` with `await`, channel type `grpc.aio.Channel`
- `sdk/python/hivekernel_sdk/syscall.py` — **NEW**: SyscallContext bridges `handle_task()` to Execute bidi stream. Each syscall (spawn, kill, send, store_artifact, get_artifact, escalate, log) creates a SystemCall protobuf with UUID call_id, puts TaskProgress(PROGRESS_SYSCALL) on asyncio.Queue, awaits Future resolved when SyscallResult arrives. Also: `report_progress()` for PROGRESS_UPDATE
- `sdk/python/hivekernel_sdk/agent.py` — HiveAgent fully async: `grpc.aio.server()`, no ThreadPoolExecutor. User methods: `async def on_init/handle_task/on_message/on_shutdown`. `handle_task(task, ctx: SyscallContext)` — new `ctx` param. Execute handler runs concurrent stream_reader/stream_writer/run_task via asyncio.create_task. New `core` property for direct CoreClient access in on_init/on_shutdown
- `sdk/python/hivekernel_sdk/__init__.py` — Exports SyscallContext
- `sdk/python/examples/test_integration.py` — All 31 checks migrated to async (`asyncio.run(main())`)
- `sdk/python/examples/echo_worker.py` — Async agent (`asyncio.run(agent.run())`)
- `sdk/python/examples/test_e2e.py` — Async (`asyncio.run(main())`)

### Go Core — Execute Stream + Syscall Dispatch

- `internal/runtime/executor.go` — **NEW**: Executor.ExecuteTask() opens bidi Execute stream to agent, sends TaskRequest, processes syscalls in a loop (PROGRESS_SYSCALL -> dispatch via SyscallHandler -> send SyscallResult back), returns TaskResult on COMPLETED/FAILED
- `internal/kernel/syscall_handler.go` — **NEW**: KernelSyscallHandler implements runtime.SyscallHandler. Dispatches spawn/kill/send/store/get_artifact/escalate/log to King subsystems (same logic as grpc_core.go but via stream)
- `cmd/hivekernel/main.go` — Wires KernelSyscallHandler + Executor into startup

### Tests: 221 passing (+11 new)
- Executor: simple complete, syscall round-trip (spawn via stream), task failure, dial error
- Syscall handler: spawn, kill (own/not-own), send message, store+get artifact, escalate, log — king -> queen -> Leo -> Architect(sleeps) + 3 Leads -> Workers/Tasks. Tree grows to 14 processes, cgroup tracks 11 members. 6 tasks submitted/assigned/completed. Token consumption: 500k across branch. Tree collapses, 0 active children remaining

---

## Phase 7 — Runtime Spawner + execute_on Syscall (completed)

**Goal:** Make the kernel actually spawn Python agent processes and enable parent->child task delegation through the Execute stream.

### Go Core

- `internal/runtime/manager.go` — **REWRITTEN**: Real OS process management. `StartRuntime()` now spawns Python process via `os/exec`, reads `READY <port>` from stdout (10s timeout), dials gRPC to agent, calls `Init()`. `StopRuntime()` sends Shutdown RPC with grace period, waits for exit (5s), force-kills if needed. Virtual process mode preserved for processes without RuntimeImage. `GetClient()` returns AgentServiceClient for a PID
- `internal/kernel/syscall_handler.go` — Added `execute_on` syscall case. `handleExecuteOn()` validates target is caller's child, gets runtime from Manager, delegates to Executor. Kill now stops runtime before marking process dead. Handler struct extended with Manager + Executor references (circular dependency resolved via `SetExecutor()`)
- `internal/kernel/king.go` — Added `rtManager` field with `SetRuntimeManager()`/`RuntimeManager()` accessors. `SpawnChild()` now auto-starts runtime when `RuntimeImage` is set
- `internal/kernel/grpc_core.go` — `KillChild()` now stops runtime before state change (both target and descendants)
- `cmd/hivekernel/main.go` — Wires Manager (with coreAddr + pythonBin), sets rtManager on King, resolves circular Handler<->Executor dependency via setter
- `api/proto/agent.proto` — Added `ExecuteOnRequest`/`ExecuteOnResponse` messages, wired into SystemCall/SyscallResult oneofs (field 9)

### Python SDK

- `sdk/python/hivekernel_sdk/runner.py` — **NEW**: Entry point for kernel-spawned agents. CLI: `python -m hivekernel_sdk.runner --agent module:Class --core addr`. Loads agent class via importlib, starts gRPC server on random port, prints `READY <port>` to stdout for Go handshake
- `sdk/python/hivekernel_sdk/__main__.py` — **NEW**: Enables `python -m hivekernel_sdk.runner`
- `sdk/python/hivekernel_sdk/syscall.py` — Added `execute_on()` method: sends ExecuteOnRequest syscall through Execute stream, returns TaskResult from child agent
- `sdk/python/examples/echo_worker.py` — Updated to demonstrate `execute_on` delegation: "delegate:X" tasks spawn a sub-worker, execute_on it, collect result

### Tests: 233 passing (+12 new)
- Runtime manager: NewManager (default python), StartRuntime virtual process, already running check, StopRuntime virtual + not-found, ListRuntimes, GetClient virtual + not-found
- execute_on syscall: successful parent->child delegation via mock agent, not-own-child rejected, no-runtime error

---

## Phase 8 — Wire RuntimeImage Through Spawn Chain (completed)

**Goal:** Ensure RuntimeImage/RuntimeType pass through all spawn entry points (gRPC, syscall handler, Python SDK).

### Changes
- `internal/kernel/grpc_core.go` — `SpawnChild()` now passes `RuntimeType` and `RuntimeImage` from proto to `SpawnRequest`
- `internal/kernel/syscall_handler.go` — `handleSpawn()` now passes `RuntimeType` and `RuntimeImage`
- `sdk/python/hivekernel_sdk/syscall.py` — `spawn()` now accepts `runtime_image` and `runtime_type` params
- `sdk/python/hivekernel_sdk/client.py` — `spawn_child()` now accepts `runtime_image` and `runtime_type` params
- `sdk/python/hivekernel_sdk/agent.py` — Init handler updates `CoreClient.pid` so subsequent RPCs carry correct PID
- `cmd/hivekernel/main.go` — Added `normalizeCoreAddr()` (":50051" -> "localhost:50051")

---

## Phase 9 — E2E Runtime Test (completed)

**Goal:** Verify the full kernel -> Python agent lifecycle works end-to-end.

### Added
- `sdk/python/examples/sub_worker.py` — Minimal agent for auto-spawn testing
- `sdk/python/examples/__init__.py` — Makes examples importable as a Python package
- `sdk/python/examples/test_runtime_e2e.py` — **10 checks**: kernel starts, spawns real Python agent, verifies process info and runtime_addr, connects to agent directly, executes task via bidi stream, verifies result, kills agent, confirms dead state
- `api/proto/core.proto` — Added `runtime_addr` field (15) to ProcessInfo
- `api/proto/agent.proto` — Added `STATE_DEAD` (4) and `STATE_ZOMBIE` (5) to AgentState enum
- `internal/kernel/convert.go` — Maps StateDead/StateZombie to proto enums, includes RuntimeAddr in ProcessInfo

---

## Phase 10 — Multi-Agent MVP (completed)

**Goal:** Full multi-agent scenario with hierarchical task delegation, runtime spawning, syscalls, and artifact storage.

### Added
- `sdk/python/examples/team_manager.py` — TeamManager agent: spawns N SubWorker agents via `ctx.spawn(runtime_image=...)`, delegates subtasks round-robin via `ctx.execute_on()`, stores combined results as artifact via `ctx.store_artifact()`, kills workers, returns summary
- `sdk/python/examples/test_team_e2e.py` — **13 checks**: spawns TeamManager (real Python process), TeamManager spawns 2 SubWorkers (real Python processes), delegates 3 subtasks, verifies all results, checks artifact stored and retrievable, verifies workers killed, cleans up manager
- `api/proto/core.proto` — Added `ExecuteTask` RPC to CoreService + `ExecuteTaskRequest`/`ExecuteTaskResponse` messages. Allows external callers to trigger task execution through the kernel's executor (which handles all syscalls)
- `internal/kernel/grpc_core.go` — Implemented `ExecuteTask()` RPC: validates target process, gets runtime, delegates to executor. Added `SetExecutor()` for wiring. CoreServer now imports `runtime` package

### Architecture validated
- **3-level hierarchy**: Kernel -> TeamManager -> 2x SubWorker (all real Python processes)
- **Full syscall chain**: spawn, execute_on, store_artifact, kill, log, report_progress
- **Recursive execution**: TeamManager's syscalls handled by Go executor, which dispatches spawn/execute_on for children
- **Artifact persistence**: Results stored in shared memory, retrievable by other processes

---

## LLM Integration (completed)

**Goal:** Real LLM calls via OpenRouter, convenience base class, live demo with web dashboard.

### Added
- `sdk/python/hivekernel_sdk/llm.py` — **NEW**: Async OpenRouter client. Zero extra deps (`urllib.request` + `asyncio.to_thread`). `LLMClient.chat()` (multi-turn), `LLMClient.complete()` (single prompt), cumulative `total_tokens` tracking. MODEL_MAP: opus=claude-opus-4, sonnet=claude-sonnet-4-5, mini=gemini-2.5-flash, haiku=claude-haiku-3-5
- `sdk/python/hivekernel_sdk/llm_agent.py` — **NEW**: `LLMAgent` extends `HiveAgent`. Auto-creates `LLMClient` in `on_init()` from `OPENROUTER_API_KEY` env var + agent's model config. Provides `ask(prompt)` and `chat(messages)` shortcuts with agent's system_prompt
- `sdk/python/hivekernel_sdk/__init__.py` — Exports `LLMClient`, `LLMAgent`
- `sdk/python/examples/llm_research_team.py` — **NEW**: Live demo with real LLM calls. LLMResearchLead (sonnet) breaks topic into subtopics via LLM, spawns 3 LLMResearchWorker agents (gemini-flash), delegates research, synthesizes findings into report. Integrated web dashboard with auto-launch + browser open + step-by-step pauses. `--no-dashboard` flag for headless mode. Built-in `.env` loader (no python-dotenv dep)
- `.env.example` — Template for OpenRouter API key
- `.gitignore` — Added `.env`

### Verified
- Full pipeline tested: lead spawns workers, real LLM calls, ~2000 tokens total
- Dashboard shows agents spawning, working, entering zombie state in real-time
