# Investigation 002: Architecture Layers & Responsibility Boundaries

**Date:** 2026-02-15
**Status:** Investigation complete, decisions documented
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

## The Gray Zones (Summary)

| Gray Zone | Currently | Issue | Recommendation |
|-----------|-----------|-------|---------------|
| **Who spawns Queen** | main.go hardcodes it | Kernel depends on Python | Config file or CLI flag |
| **Who spawns Queen's children** | Queen.on_init() hardcodes them | Can't change without editing code | Config or params |
| **Cron execution** | Go kernel CronScheduler | Spec says "Queen holds crontab" | Keep in kernel (reliability), rename to reflect |
| **Cron management** | gRPC AddCron/RemoveCron | Who decides what to schedule? | API is fine, decisions are agent-side |
| **Accountant** | Kernel records usage | Only records, doesn't enforce | Keep (low overhead, no lost events) |
| **Scheduler** | Kernel task queue | Not used by agents yet | Keep but clarify role |
| **Migration decisions** | Kernel handleOverloadEscalation | Business logic in kernel | Move decision to agent |
| **Maid (Go version)** | internal/daemons/maid.go | Duplicated by Python MaidAgent | Remove Go maid or Python maid |
| **Supervisor restart** | Kernel decides restart policy | Policy could be agent-configurable | Keep mechanism in kernel, make policy configurable |

---

## The Maid Duplication Problem

There are TWO Maids:

1. **Go Maid** (`internal/daemons/maid.go`) — per-VPS health daemon,
   scans for zombies, monitors memory, reports to parent via message queue.
   Runs inside the kernel process.

2. **Python MaidAgent** (`sdk/python/hivekernel_sdk/maid.py`) — LLM-powered
   health agent, spawned by Queen as a separate Python process.
   Communicates via syscalls.

They do similar things but differently:
- Go Maid: lightweight, no LLM, runs in kernel process
- Python Maid: heavier, LLM-capable, separate process

**Recommendation:** Keep ONE. The Python MaidAgent is more capable and follows
the agent architecture. The Go Maid was created early (Phase 1) before
Python agents worked. It should probably be removed.

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

## What Needs to Change

### Definite Changes

1. **Remove hardcoded Queen spawn from main.go**
   - Kernel should start clean (just King PID 1)
   - Agent spawning is controlled by config or external caller

2. **Make Queen's daemon list configurable**
   - Queen.on_init() should read from config/params
   - Not hardcode Maid + Assistant + GitMonitor + Coder

3. **Remove Go Maid** (`internal/daemons/maid.go`)
   - Replaced by Python MaidAgent
   - Or vice versa — but only keep ONE

### Debatable Changes

4. **Move migration DECISIONS to agent**
   - Keep MigrationManager mechanism in kernel
   - Move "when to migrate" logic to a ClusterAgent

5. **Clarify Scheduler role**
   - Currently not used by any agent
   - Either wire it up or remove dead code

6. **PYTHONPATH auto-detection**
   - Kernel should find SDK automatically
   - Or accept `--sdk-path` flag

---

## Appendix: Complete Kernel Subsystem Inventory

### King owns 29 subsystems:

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
| 18 | Accountant | resources | Infrastructure | KERNEL (gray) |
| 19 | CGroupManager | resources | Infrastructure | KERNEL |
| 20 | Scheduler | scheduler | Infrastructure | KERNEL (gray) |
| 21 | CronScheduler | scheduler | Infrastructure | KERNEL (gray) |
| 22 | NodeRegistry | cluster | Infrastructure | KERNEL (gray) |
| 23 | Connector | cluster | Infrastructure | KERNEL (gray) |
| 24 | MigrationManager | cluster | Infrastructure | GRAY ZONE |
| 25 | RuntimeManager | runtime | Bridge | KERNEL |
| 26 | Executor | runtime | Bridge | KERNEL |
| 27 | HealthMonitor | runtime | Bridge | KERNEL |
| 28 | Config | kernel | Core | KERNEL |
| 29 | Process (self) | process | Core | KERNEL |

**Core kernel (pure OS primitives):** 11 subsystems
**Infrastructure (policy + scheduling):** 13 subsystems
**Runtime bridge:** 3 subsystems
**Gray zones:** 5 subsystems (accountant, scheduler, cron, cluster)
**Violations:** 2 (hardcoded Queen in main.go, Go Maid duplication)
