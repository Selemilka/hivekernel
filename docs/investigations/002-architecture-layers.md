# Investigation 002: Architecture Layers & Responsibility Boundaries

**Date:** 2026-02-15
**Status:** COMPLETE (implemented in Plan 004)
**Related:** Investigation 001 (Kernel-Agent Coupling)

---

## Goal

Define clear boundaries: what is the kernel, what are agents, and where
the gray zones are. Every component should have a single clear owner.

---

## The Layered Architecture

```
+==================================================================+
|  Layer 5: APPLICATION AGENTS (Python)                            |
|  Queen, Assistant, GitMonitor, Coder, Orchestrator, Worker, etc. |
|  --- business logic, LLM calls, user-facing features ---         |
+==================================================================+
|  Layer 4: RUNTIME BRIDGE (Go)                                    |
|  Manager, Executor, HealthMonitor                                |
|  --- spawn processes, gRPC dial, task dispatch ---               |
+==================================================================+
|  Layer 3: INFRASTRUCTURE (Go)                                    |
|  Scheduler, Cron, Cluster, Permissions, Resources                |
|  --- policy, scheduling, multi-VPS, budgets ---                  |
+==================================================================+
|  Layer 2: CORE KERNEL PRIMITIVES (Go)                            |
|  Registry, Spawner, Signals, Lifecycle, Broker, SharedMem,       |
|  Pipes, EventBus, EventLog, Supervisor                           |
|  --- process table, IPC, signals, event sourcing ---             |
+==================================================================+
|  Layer 1: SYSCALL INTERFACE (gRPC proto)                         |
|  10 syscalls: spawn, kill, send, store, get_artifact,            |
|  escalate, log, execute_on, wait_child, report_progress          |
|  --- the contract between kernel and agents ---                  |
+==================================================================+
```

---

## Layer 1: Syscall Interface (The Foundation)

The 10 syscalls are the **contract** between kernel and agents.
An agent can ONLY interact with the kernel through these syscalls.
This is clean and well-defined.

| # | Syscall | Category | What It Does | Touches Other Agents? |
|---|---------|----------|--------------|----------------------|
| 1 | `spawn` | Process mgmt | Create child process | No |
| 2 | `kill` | Process mgmt | Terminate child (-> zombie) | No |
| 3 | `send` | IPC | Route message to another process | Yes (delivery) |
| 4 | `store_artifact` | Storage | Store data in shared memory | No |
| 5 | `get_artifact` | Storage | Retrieve data from shared memory | No |
| 6 | `escalate` | Signaling | Send issue to parent | Yes (parent) |
| 7 | `log` | Observability | Write log entry + event | No |
| 8 | `execute_on` | Task delegation | Run task on child agent | Yes (child) |
| 9 | `wait_child` | Process mgmt | Wait for child zombie, collect result | No |
| 10 | `report_progress` | Observability | Send progress update during task | No |

**Verdict: CLEAN.** No agent-specific logic in any syscall handler.
The handler dispatches to kernel primitives (registry, broker, shared memory)
without knowing what kind of agent is calling.

### Additionally: gRPC CoreService RPCs

Beyond the 10 in-stream syscalls, the kernel exposes gRPC RPCs that external
callers (dashboard, CLI tools, other kernels) can use directly:

| RPC | Purpose | Who Calls It |
|-----|---------|-------------|
| SpawnChild | Create child process | Dashboard, CLI, other kernels |
| KillChild | Kill process | Dashboard, CLI |
| GetProcessInfo | Query process state | Dashboard, any caller |
| ListChildren | List descendants | Dashboard |
| SendMessage | Route message | Any caller |
| Subscribe | Stream messages from inbox | Long-lived listeners |
| SubscribeEvents | Stream process events | Dashboard (event sourcing) |
| StoreArtifact / GetArtifact / ListArtifacts | Shared memory | Any caller |
| GetResourceUsage / RequestResources | Budget queries | Agents, dashboard |
| Escalate | Push escalation | Any caller |
| Log / ReportMetric | Observability | Agents |
| ExecuteTask | Run task on agent | Dashboard, cron poller |
| AddCron / RemoveCron / ListCron | Cron management | Dashboard, agents |

**Verdict: MOSTLY CLEAN.** These are kernel-level operations.
The only question is whether cron management belongs here (see gray zones).

---

## Layer 2: Core Kernel Primitives

These are the OS-level primitives. Like a real kernel, they manage processes,
memory, signals, and communication. Agents don't need to know about them.

| Package | Component | What It Does | Verdict |
|---------|-----------|-------------|---------|
| process | **Registry** | Process table: register, lookup, tree traversal, NCA | KERNEL |
| process | **Spawner** | Process creation with validation (tier, role, limits) | KERNEL |
| process | **SignalRouter** | SIGTERM, SIGKILL, SIGCHLD, SIGSTOP, SIGCONT delivery | KERNEL |
| process | **LifecycleManager** | Sleep/wake, CompleteTask, WaitResult, CollapseBranch | KERNEL |
| process | **Supervisor** | Restart policies, zombie reaping, crash detection | KERNEL |
| process | **TreeOps** | KillBranch (bottom-up), GetDescendants | KERNEL |
| process | **EventLog** | Event sourcing: ring buffer + JSONL disk + subscriber fan-out | KERNEL |
| ipc | **Broker** | Message routing with priority aging, routing rules | KERNEL |
| ipc | **PriorityQueue** | Heap-based queue with aging (prevents starvation) | KERNEL |
| ipc | **SharedMemory** | Artifact store with visibility (private/user/subtree/global) | KERNEL |
| ipc | **PipeRegistry** | Bidirectional parent-child byte channels | KERNEL |
| ipc | **EventBus** | Pub/sub for system events (budget_exceeded, migration, etc.) | KERNEL |

**Verdict: ALL CLEAN.** These are pure OS primitives. No agent logic.

---

## Layer 3: Infrastructure

This is where it gets interesting. These components provide policy,
scheduling, and resource management. They're currently in the kernel,
and most SHOULD stay there, but some could theoretically be agents.

### 3a. Permissions (STAYS IN KERNEL)

| Component | What It Does | Why Kernel |
|-----------|-------------|-----------|
| **AuthProvider** | USER identity resolution, inheritance validation | Needs process table access |
| **ACL** | Role-based access control for syscalls | Must enforce BEFORE syscall executes |
| **CapabilityChecker** | Role-based capability validation | Must enforce BEFORE spawn/kill |

**Verdict: KERNEL.** These are enforcement mechanisms. If they were agents,
a malicious agent could bypass them. Security enforcement must be in kernel.

### 3b. Resources (MOSTLY KERNEL)

| Component | What It Does | Where It Belongs |
|-----------|-------------|-----------------|
| **BudgetManager** | Token allocation tree (parent -> child) | KERNEL (enforces limits on syscalls) |
| **RateLimiter** | Message rate limiting per PID | KERNEL (enforces on send syscall) |
| **LimitChecker** | Spawn/context/timeout limits | KERNEL (enforces on spawn syscall) |
| **Accountant** | Records token usage by user/VPS/PID | GRAY ZONE |
| **CGroupManager** | Group resource limits by branch | KERNEL (enforces collective limits) |

**Accountant** is the one gray zone here. It only *records* usage, doesn't
*enforce* anything. Could be an agent daemon that reads events. But keeping
it in kernel means zero-latency accounting with no lost events.

### 3c. Scheduling (GRAY ZONE)

| Component | What It Does | Where It Belongs |
|-----------|-------------|-----------------|
| **Scheduler** | Task queue with priority aging, assignment | GRAY ZONE |
| **CronScheduler** | Cron expression parsing, due detection | GRAY ZONE |
| **King.RunCronPoller** | 30s ticker that fires due cron entries | GRAY ZONE |

This is the biggest gray zone. The spec says:

> "Scheduling is a core responsibility, not agent's. Queen holds the crontab
> per VPS, triggers tasks on schedule." (line 158)

> "Cron is core responsibility. Agents may be sleeping/unspawned. Queen holds
> crontab, wakes or spawns agents on schedule." (line 1068)

**The contradiction:**
- Spec says scheduling is "core" but "Queen holds crontab"
- We implemented CronScheduler in Go kernel (correct for "core")
- But spec says Queen should hold it (agent-side)

**The practical argument for KERNEL:**
- Cron must fire even if Queen is crashed/restarting
- Kernel is always running; agents may not be
- If Queen holds crontab and Queen crashes, all schedules stop

**The practical argument for AGENT:**
- Different VPS might have different schedules
- Adding/removing cron entries is business logic, not OS logic
- A real OS doesn't have cron in the kernel (it's a userspace daemon)

**Our current approach:** Cron parsing and execution in Go kernel,
cron management API (AddCron/RemoveCron) as gRPC RPCs. This is like
Linux: crond is a daemon, but the kernel provides timers.

**Recommendation:** Keep cron execution in kernel (reliable timer),
but acknowledge it's infrastructure, not a core primitive.

### 3d. Clustering (GRAY ZONE)

| Component | What It Does | Where It Belongs |
|-----------|-------------|-----------------|
| **NodeRegistry** | Track all VPS nodes, health, load | GRAY ZONE |
| **Connector** | TCP/TLS connections to other VPS kernels | GRAY ZONE |
| **MigrationManager** | Process branch migration between VPS | GRAY ZONE |

Currently all in kernel. Not yet used in production.

**Argument for kernel:** Core needs to know node topology for routing.
**Argument for agent:** Migration *decisions* are business logic;
a ClusterManager agent could decide when/where to migrate.

**Recommendation:** Keep NodeRegistry and Connector in kernel (plumbing),
move migration *decisions* to an agent (MigrationManager stays as mechanism,
but the "should we migrate?" logic goes to an agent).

---

## Layer 4: Runtime Bridge

This is the thin boundary between kernel (Go) and agents (Python/Claw/any).

| Component | What It Does | Where It Belongs |
|-----------|-------------|-----------------|
| **Manager** | Spawn OS processes, READY protocol, gRPC dial, Init, Stop | KERNEL |
| **Executor** | Execute bidi stream: send task, handle syscalls, collect result | KERNEL |
| **HealthMonitor** | Ping agents, detect failures, invoke callback | KERNEL |

**Verdict: ALL KERNEL.** The runtime bridge MUST be in the kernel because:
- Only the kernel can spawn OS processes
- Only the kernel can route syscalls during execution
- Health monitoring feeds into Supervisor (kernel)

The bridge is already well-abstracted:
- `RuntimeType` supports multiple runtimes (Python, future Claw)
- `RuntimeImage` is a string — kernel doesn't parse it
- The bridge is thin: spawn, dial, execute, stop

---

## Layer 5: Application Agents

Everything in `sdk/python/hivekernel_sdk/` is application logic.
The kernel should NOT depend on any of it.

| Agent | What It Does | Depends on Kernel? |
|-------|-------------|-------------------|
| **QueenAgent** | Task routing (complexity assessment + dispatch) | Yes (syscalls only) |
| **AssistantAgent** | Chat with users, schedule cron, delegate to Coder | Yes (syscalls only) |
| **GitHubMonitorAgent** | Check GitHub API, compare with stored state | Yes (syscalls only) |
| **CoderAgent** | Generate code via LLM, store as artifact | Yes (syscalls only) |
| **OrchestratorAgent** | Decompose tasks, manage workers | Yes (syscalls only) |
| **WorkerAgent** | Execute individual subtasks via LLM | Yes (syscalls only) |
| **ArchitectAgent** | Strategic planning for complex tasks | Yes (syscalls only) |
| **MaidAgent** | Health monitoring, anomaly detection | Yes (syscalls only) |

**Verdict: ALL APPLICATION.** These should have ZERO presence in kernel code.
The kernel should not import, reference, or know about any of these classes.

---

## Gray Zone Resolutions

All gray zones have been resolved. Summary of decisions:

| Gray Zone | Decision | Rationale |
|-----------|----------|-----------|
| **Cron** | **KERNEL** | Agents request cron via RPC; kernel holds crontab and fires reliably even if agents crash |
| **Cron management** | **KERNEL API** | AddCron/RemoveCron are kernel RPCs; agents decide *what* to schedule, kernel decides *when* to fire |
| **Accountant** | **KERNEL** | Zero-latency recording, no lost events; doesn't enforce, just records |
| **Scheduler** | **KERNEL** | Task queue with priority aging is an OS primitive; agents submit tasks, kernel orders them |
| **Clustering: mechanism** | **KERNEL** | NodeRegistry, Connector, MigrationManager stay in kernel — plumbing |
| **Clustering: decisions** | **AGENT** | "When/where to migrate" is business logic; agent requests migration via syscall, kernel executes |
| **Go Maid** | **DELETE** | Supervisor + HealthMonitor already do zombie reaping and crash detection; Go Maid is dead code |
| **Python MaidAgent** | **KEEP (optional)** | LLM-powered anomaly detection is application logic (Layer 5); not part of kernel |
| **Supervisor restart** | **KERNEL** | Policy is derived from role (daemon=always, task=never, agent=notify); mechanism stays in kernel |
| **Who spawns Queen** | **CONFIG/CLI** | Remove hardcoded spawnQueen() from main.go; kernel starts clean |
| **Queen's daemon spawns** | **CONFIG** | Remove hardcoded on_init() spawns; read from config or startup params |

### Decision 4: Cron Is a Kernel Primitive

Cron stays in the kernel (Layer 3 Infrastructure). This is the right place because:

1. **Reliability** — agents crash; kernel doesn't. Cron must fire on schedule.
2. **Clean API** — agents use `AddCron`/`RemoveCron`/`ListCron` RPCs to manage schedules.
   They don't need their own timers or polling loops.
3. **Separation** — agents decide *what* to schedule (business logic).
   Kernel decides *when* to fire (timer infrastructure).

Linux analogy: `crond` is a userspace daemon, but in HiveKernel agents ARE the
userspace — and they can't be trusted to stay alive. Kernel-side cron is more
like a watchdog timer in an embedded OS.

### Decision 5: Clustering — Mechanism in Kernel, Decisions in Agents

**Kernel provides (Layer 3):**
- `NodeRegistry` — knows which VPS nodes exist, their health/load
- `Connector` — TCP/TLS plumbing between VPS kernels
- `MigrationManager` — snapshot → transfer → restore mechanism

**Agents decide (Layer 5):**
- *When* to migrate (load threshold, user request, etc.)
- *Where* to migrate (which VPS, based on business criteria)
- Request migration via syscall; kernel executes blindly

**Current violation:** `King.handleOverloadEscalation()` contains migration
decision logic ("find least loaded node, migrate there"). This should move to
an agent. King should only expose a `migrate(branch_pid, target_vps)` mechanism.

**Future:** Add `migrate` syscall so agents can request migrations directly.

### Decision 6: Queen = Task Dispatcher (Not System Daemon)

Queen has two unrelated responsibilities mixed together:

**A. Daemon Spawner (on_init) — REMOVE from Queen**

```
on_init():
  spawn Maid, Assistant, GitMonitor, Coder
  schedule github-check cron
```

This is **startup configuration**, not agent logic. It belongs in a config file
or startup script, not hardcoded in a Python class. Removing it makes Queen
a pure optional agent that the kernel doesn't depend on.

**B. Task Dispatcher (handle_task) — KEEP in Queen**

```
handle_task(task):
  assess complexity (history -> heuristics -> LLM)
  route: simple | complex | architect
  manage leads (idle pool, reaper)
  store result
```

This IS Queen's real job: LLM-powered task routing with 3-tier strategy.
It's application logic (Layer 5), uses only syscalls, and is fully optional.

**After cleanup, Queen is:**
- An optional Python agent (not required for kernel to run)
- A task dispatcher with LLM-based complexity assessment
- A lead pool manager (reuse orchestrators between tasks)
- Spawned by config/CLI, not hardcoded in main.go

### Decision 7: Go Maid Deleted, HealthMonitor Stays

| Component | Layer | Action |
|-----------|-------|--------|
| `internal/daemons/maid.go` (Go) | Kernel | **DELETE** — superseded by Supervisor + HealthMonitor |
| `internal/runtime/health.go` (Go) | Layer 4 (Bridge) | **KEEP** — pings agent runtimes, detects failures |
| `process/supervisor.go` (Go) | Layer 2 (Core) | **KEEP** — zombie reaping, restart policies, crash handling |
| `hivekernel_sdk/maid.py` (Python) | Layer 5 (Agent) | **KEEP** — optional LLM anomaly detection agent |

Go Maid was created in Phase 1 before Python agents worked. Its responsibilities
(zombie scanning, health reporting) are now handled by:
- **Supervisor** — zombie reaping (15s scan, 60s timeout)
- **HealthMonitor** — runtime ping (10s interval, callback on failure)
- **EventLog** — all state changes logged for observability

Python MaidAgent adds LLM-powered analysis on top, which is application logic.

---

## Clean Architecture Definition

Based on the analysis, here's what each part IS:

### Kernel (Go binary)

**What it is:** A process-tree OS runtime. Like Linux, but for LLM agents.

**What it does:**
- Manages processes (create, kill, state transitions, zombie lifecycle)
- Routes messages between processes (IPC with priority aging)
- Stores shared artifacts (key-value with visibility)
- Delivers signals (SIGTERM, SIGKILL, SIGCHLD, SIGSTOP, SIGCONT)
- Enforces security (ACL, capabilities, user inheritance)
- Manages resources (token budgets, rate limits, cgroups)
- Sources events (ring buffer + JSONL disk + subscriber stream)
- Bridges runtimes (spawn Python/Claw processes, gRPC execute)
- Runs cron (timer-based task triggering)
- Schedules tasks (priority queue with aging)

**What it does NOT do:**
- Make business decisions (what to spawn, how to route tasks)
- Call LLMs
- Talk to external APIs (GitHub, etc.)
- Know about specific agent implementations
- Have hardcoded agent spawn lists

### Agents (Python processes)

**What they are:** Pluggable processes that run business logic.

**What they do:**
- Call LLMs (OpenRouter, etc.)
- Make decisions (task complexity, execution strategy)
- Interact with external services (GitHub API, etc.)
- Spawn child agents (via syscalls)
- Store and retrieve data (via syscalls)
- Communicate with each other (via syscalls)

**What they do NOT do:**
- Modify the process table directly
- Route messages for other agents
- Enforce security policies
- Manage other agents' runtimes

### Dashboard (Python FastAPI)

**What it is:** A web UI for humans to observe and interact with the system.

**What it does:**
- Visualizes the process tree (D3.js)
- Subscribes to kernel events (SubscribeEvents gRPC stream)
- Provides chat interface (delegates to Assistant agent via ExecuteTask)
- Manages cron entries (via AddCron/RemoveCron gRPC)
- Spawn/kill/execute agents manually

**What it does NOT do:**
- Make autonomous decisions
- Run as an agent in the process tree
- Depend on specific agents existing

---

## Key Decisions

### Decision 1: King = init(1)

King is NOT a "king of agents" — he is **init(1)**, the root process of the OS.

| Linux | HiveKernel | Role |
|-------|-----------|------|
| init / systemd (PID 1) | King (PID 1) | Root process, owns all subsystems |
| fork(2) / exec(3) | spawn syscall | Create child processes |
| waitpid(2) | wait_child syscall | Collect child exit status |
| kill(2) | kill syscall | Send signal to process |
| crond (userspace) | CronScheduler (kernel) | Timer-based execution |
| No analogy | Queen | Optional application agent |

**Naming decision:** Keep `King` in code. Renaming to `Init` conflicts with Go's
`init()` function. `Kernel` creates `kernel.Kernel` redundancy. `Core` conflicts
with `CoreService`. King is unique, unambiguous, and already established across
37 source files and 30 test files.

**Documentation rule:** Always refer to King as "init(1) / root process", never
as "leader" or "boss" of agents. King doesn't know agents exist.

### Decision 2: Zombie Lifecycle Is Already Correct

The zombie lifecycle is **entirely on Layer 2 (Core Kernel Primitives)**.
No Python agent participates in zombie management. This is clean.

**Paths to Zombie state (4):**

| Path | Function | Layer |
|------|----------|-------|
| Task completes | `LifecycleManager.CompleteTask()` | Layer 2 |
| Child crashes | `Supervisor.HandleChildExit()` | Layer 2 |
| Parent kills child | gRPC `KillChild` / syscall `kill` | Layer 1 → Layer 2 |
| Branch collapse | `LifecycleManager.CollapseBranch()` | Layer 2 (skips zombie → Dead) |

**Zombie reaping:**
- `Supervisor.reapZombies()` — 15s scan interval, 60s timeout
- Reparents orphaned children to grandparent (Unix semantics)
- Fires `EventRemoved` for dashboard

**SIGCHLD delivery:**
- `SignalRouter.NotifyParent()` — sent on every death path
- Parent can collect result via `wait_child` syscall (polls 200ms)

**What about Python Maid?** Python MaidAgent *scans* for zombies and *escalates*
to King, but does NOT reap them. Real reaping is Go Supervisor only.
This makes Python Maid redundant for zombie cleanup — Supervisor already does it.
Python Maid's value (if any) is in LLM-powered anomaly detection, not zombie reaping.

### Decision 3: What King Is and Is Not

**King IS:**
- A facade over 23 kernel subsystems (Registry, Broker, Budget, ACL, ...)
- The `New()` constructor that wires everything together
- The `Run()` main loop that dispatches messages from inbox
- The `SpawnChild()` pipeline with 7-step validation
- **init(1)** — always exists, always PID 1, always running

**King is NOT:**
- An agent (he doesn't call LLMs, doesn't make business decisions)
- A manager of specific agents (he doesn't know about Queen, Maid, etc.)
- Replaceable — without him there's no process table, no IPC, no signals

**King's 26 accessor methods** are just thin wrappers (`func (k *King) Registry()
*process.Registry`). gRPC CoreServer uses them to reach subsystems. King adds no
logic on top — the subsystems are the real architecture.

---

## Roadmap: Code Changes

### Phase R1: Decouple Kernel from Python (main.go cleanup)

**Goal:** Kernel starts clean — just King (PID 1), gRPC server, no agents.

| # | File | Change | Risk |
|---|------|--------|------|
| 1 | `cmd/hivekernel/main.go` | Remove `spawnQueen()` function and its goroutine call | Low |
| 2 | `cmd/hivekernel/main.go` | Add `--startup` flag: path to YAML config file | Low |
| 3 | `cmd/hivekernel/main.go` | Add startup config loader: read YAML, spawn listed agents | Medium |
| 4 | NEW `configs/startup.yaml` | Default config: empty (pure kernel) | Low |
| 5 | NEW `configs/startup-full.yaml` | Example: queen + daemons (opt-in demo mode) | Low |

**After R1:** `bin/hivekernel.exe --listen :50051` starts pure kernel.
`bin/hivekernel.exe --listen :50051 --startup configs/startup-full.yaml` starts with agents.

**Tests:** All existing Go tests pass (they don't depend on spawnQueen).
New test: kernel starts without `--startup`, no Python processes spawned.

### Phase R2: Queen cleanup (remove daemon spawning)

**Goal:** Queen is a pure task dispatcher. No hardcoded on_init() spawns.

| # | File | Change | Risk |
|---|------|--------|------|
| 1 | `sdk/python/hivekernel_sdk/queen.py` | Remove `_spawn_maid()`, `_spawn_assistant()`, `_spawn_github_monitor()`, `_spawn_coder()` | Medium |
| 2 | `sdk/python/hivekernel_sdk/queen.py` | Remove the 4 `asyncio.create_task()` calls from `on_init()` | Low |
| 3 | `sdk/python/hivekernel_sdk/queen.py` | Remove `_maid_pid`, `_assistant_pid`, `_gitmonitor_pid`, `_coder_pid` fields | Low |
| 4 | `sdk/python/hivekernel_sdk/queen.py` | Update docstring to reflect "task dispatcher" role | Low |
| 5 | `configs/startup-full.yaml` | List all agents: queen, maid, assistant, gitmonitor, coder | Low |

**After R2:** Queen only does: assess complexity -> route to strategy -> manage leads.
All daemon spawning is in YAML config, not Python code.

**Tests:** Queen unit tests pass (mock task handling, no spawn side effects).

### Phase R3: Delete Go Maid

**Goal:** Remove dead code. HealthMonitor + Supervisor cover everything.

| # | File | Change | Risk |
|---|------|--------|------|
| 1 | `internal/daemons/maid.go` | **DELETE** | Low |
| 2 | `internal/daemons/maid_test.go` | **DELETE** (if exists) | Low |
| 3 | Any imports of `daemons` package | Remove if no other daemons remain | Low |

**After R3:** `internal/daemons/` directory removed or empty.
Zombie reaping: Supervisor. Runtime health: HealthMonitor. Anomaly detection: Python MaidAgent.

**Tests:** All Go tests pass (maid tests removed, others unaffected).

### Phase R4: PYTHONPATH auto-detection

**Goal:** Kernel finds Python SDK automatically.

| # | File | Change | Risk |
|---|------|--------|------|
| 1 | `cmd/hivekernel/main.go` | Add `--sdk-path` flag (default: auto-detect) | Low |
| 2 | `internal/runtime/manager.go` | Auto-detect SDK: check `./sdk/python`, `../sdk/python`, env `HIVEKERNEL_SDK_PATH` | Medium |
| 3 | `internal/runtime/manager.go` | Set PYTHONPATH in spawned process env automatically | Low |

**After R4:** `bin/hivekernel.exe --listen :50051 --startup configs/startup-full.yaml`
works without manual PYTHONPATH. SDK found automatically relative to binary.

### Phase R5: Migration syscall

**Goal:** Agents can request branch migration via syscall.

| # | File | Change | Risk |
|---|------|--------|------|
| 1 | `api/proto/core.proto` | Add `migrate` syscall message in ExecuteRequest | Medium |
| 2 | `internal/kernel/syscall_handler.go` | Add `handleMigrate()` — validate ownership, delegate to MigrationManager | Medium |
| 3 | `internal/kernel/king.go` | Remove business logic from `handleOverloadEscalation()` — just log, let agents decide | Low |
| 4 | `sdk/python/hivekernel_sdk/syscall.py` | Add `ctx.migrate(branch_pid, target_vps)` | Low |

**After R5:** Migration decisions are agent-side. Kernel only provides the mechanism.

### Phase R6: Startup config format

**Goal:** Define the YAML schema for agent startup configuration.

```yaml
# configs/startup-full.yaml
agents:
  - name: queen
    role: daemon
    cognitive_tier: tactical
    model: sonnet
    runtime_type: python
    runtime_image: hivekernel_sdk.queen:QueenAgent
    system_prompt: "You are a task dispatcher..."

  - name: maid@local
    role: daemon
    cognitive_tier: operational
    runtime_type: python
    runtime_image: hivekernel_sdk.maid:MaidAgent

  - name: assistant
    role: daemon
    cognitive_tier: tactical
    model: sonnet
    runtime_type: python
    runtime_image: hivekernel_sdk.assistant:AssistantAgent
    system_prompt: "You are HiveKernel Assistant..."

  - name: github-monitor
    role: daemon
    cognitive_tier: operational
    runtime_type: python
    runtime_image: hivekernel_sdk.github_monitor:GitHubMonitorAgent
    cron:
      - name: github-check
        expression: "*/30 * * * *"
        description: "Check GitHub repos for updates"

  - name: coder
    role: daemon
    cognitive_tier: tactical
    model: sonnet
    runtime_type: python
    runtime_image: hivekernel_sdk.coder:CoderAgent
```

This replaces both `spawnQueen()` in main.go AND `Queen.on_init()` daemon spawning.
Queen is just another agent in the list — not the spawner of others.

### Implementation Order

```
R1 (decouple main.go)     -- highest priority, unblocks everything
R3 (delete Go Maid)        -- easy cleanup, no dependencies
R4 (PYTHONPATH)            -- quality of life
R2 (Queen cleanup)         -- depends on R1 (config format must exist first)
R6 (config format)         -- design work, parallel with R1
R5 (migrate syscall)       -- lowest priority, clustering not used yet
```

Phases R1 + R3 can be done immediately with minimal risk.
R2 + R6 should be done together (Queen needs config to replace hardcoded spawns).
R5 is future work (clustering is not production-ready).

---

## Appendix: Complete Kernel Subsystem Inventory (Updated)

### King owns 28 subsystems (after Go Maid removal):

| # | Subsystem | Package | Layer | Verdict |
|---|-----------|---------|-------|---------|
| 1 | Registry | process | Core | KERNEL |
| 2 | Spawner | process | Core | KERNEL |
| 3 | SignalRouter | process | Core | KERNEL |
| 4 | LifecycleManager | process | Core | KERNEL |
| 5 | Supervisor | process | Core | KERNEL |
| 6 | EventLog | process | Core | KERNEL |
| 7 | Inbox (PriorityQueue) | ipc | Core | KERNEL |
| 8 | Broker | ipc | Core | KERNEL |
| 9 | SharedMemory | ipc | Core | KERNEL |
| 10 | PipeRegistry | ipc | Core | KERNEL |
| 11 | EventBus | ipc | Core | KERNEL |
| 12 | AuthProvider | permissions | Infrastructure | KERNEL |
| 13 | ACL | permissions | Infrastructure | KERNEL |
| 14 | CapabilityChecker | permissions | Infrastructure | KERNEL |
| 15 | BudgetManager | resources | Infrastructure | KERNEL |
| 16 | RateLimiter | resources | Infrastructure | KERNEL |
| 17 | LimitChecker | resources | Infrastructure | KERNEL |
| 18 | Accountant | resources | Infrastructure | KERNEL |
| 19 | CGroupManager | resources | Infrastructure | KERNEL |
| 20 | Scheduler | scheduler | Infrastructure | KERNEL |
| 21 | CronScheduler | scheduler | Infrastructure | KERNEL |
| 22 | NodeRegistry | cluster | Infrastructure | KERNEL |
| 23 | Connector | cluster | Infrastructure | KERNEL |
| 24 | MigrationManager | cluster | Infrastructure | KERNEL (mechanism only) |
| 25 | RuntimeManager | runtime | Bridge | KERNEL |
| 26 | Executor | runtime | Bridge | KERNEL |
| 27 | HealthMonitor | runtime | Bridge | KERNEL |
| 28 | Config | kernel | Core | KERNEL |

**Core kernel (pure OS primitives):** 11 subsystems
**Infrastructure (policy + scheduling):** 13 subsystems
**Runtime bridge:** 3 subsystems
**Config:** 1 subsystem
**Gray zones:** 0 (all resolved)
**Violations to fix:** 2 (hardcoded Queen in main.go, Go Maid duplication)
