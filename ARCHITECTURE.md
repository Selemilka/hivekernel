# HiveKernel Architecture

> A Linux-inspired runtime for managing swarms of LLM agents.
> Go core + gRPC + Python agent runtime.

## Table of Contents

1. [Overview](#1-overview)
2. [Process Model](#2-process-model)
3. [IPC System](#3-ipc-system)
4. [Resource Management](#4-resource-management)
5. [Permissions](#5-permissions)
6. [Scheduling](#6-scheduling)
7. [Runtime](#7-runtime)
8. [Cluster](#8-cluster)
9. [gRPC API Reference](#9-grpc-api-reference)
10. [Python SDK Reference](#10-python-sdk-reference)

---

## 1. Overview

HiveKernel models LLM agent orchestration after the Linux process model:
agents are processes in a tree, with the kernel (PID 1) at the root.

### Three-Layer Architecture

```
+-----------------------------------------------------+
|                   External Callers                    |
|           (Python scripts, other services)            |
+---------------------------+--------------------------+
                            | gRPC (ExecuteTask, SpawnChild, ...)
                            v
+-----------------------------------------------------+
|                    Go Core (King)                     |
|  +--------+ +--------+ +-------+ +--------+ +-----+ |
|  |Registry| | Broker | |Budget | |  ACL   | |Sched| |
|  +--------+ +--------+ +-------+ +--------+ +-----+ |
|  |Spawner | |SharedMem| |CGroups| | Caps  | | Cron| |
|  +--------+ +--------+ +-------+ +--------+ +-----+ |
|  |Lifecycle| | Pipes  | |Acctng | | Auth  | |Migr. | |
|  +--------+ +--------+ +-------+ +--------+ +-----+ |
+---------------------------+--------------------------+
                            | gRPC (Init, Execute, Shutdown, ...)
                            v
+-----------------------------------------------------+
|                Python Agent Runtime                   |
|  +----------+ +----------+ +---------+ +---------+  |
|  | HiveAgent| | Syscall  | |  Core   | | Runner  |  |
|  |  (base)  | | Context  | | Client  | |         |  |
|  +----------+ +----------+ +---------+ +---------+  |
+-----------------------------------------------------+
```

### Design Philosophy

- **Process tree**: Every agent is a process with PID, PPID, role, cognitive tier, state
- **Hierarchical control**: Parent spawns, delegates, kills children. No peer-to-peer.
- **Cognitive tiers**: Strategic (opus) > Tactical (sonnet) > Operational (mini). Child tier <= parent tier.
- **Syscalls over stream**: Agents perform kernel operations (spawn, kill, send, store) via bidirectional gRPC stream during task execution
- **Budget inheritance**: Token budgets flow down the tree. Parent allocates to child from its own pool.
- **USER inheritance**: All children share the parent's user identity (except kernel can assign different users)

### Key Source Files

| File | Description |
|------|-------------|
| `cmd/hivekernel/main.go` | Entry point, gRPC server setup, demo scenario |
| `internal/kernel/king.go` | King (PID 1), owns all subsystems |
| `internal/kernel/grpc_core.go` | gRPC CoreService implementation |
| `internal/kernel/syscall_handler.go` | Dispatches in-stream syscalls to subsystems |
| `api/proto/agent.proto` | Agent service + common types + syscalls |
| `api/proto/core.proto` | Core service definition |
| `sdk/python/hivekernel_sdk/agent.py` | HiveAgent base class |
| `sdk/python/hivekernel_sdk/syscall.py` | SyscallContext for in-stream syscalls |

---

## 2. Process Model

Every entity in HiveKernel is a **process** in a tree rooted at PID 1 (king).

### Process Struct

```go
// internal/process/types.go
type Process struct {
    PID           PID            // unique process identifier (uint64)
    PPID          PID            // parent PID (0 for kernel)
    User          string         // inherited from parent
    Name          string         // human-readable name
    Role          AgentRole      // kernel/daemon/agent/architect/lead/worker/task
    CognitiveTier CognitiveTier  // strategic/tactical/operational
    Model         string         // LLM model (opus/sonnet/mini)
    VPS           string         // which VPS node this runs on
    State         ProcessState   // idle/running/blocked/sleeping/zombie/dead
    Limits        ResourceLimits // per-process constraints
    TokensConsumed      uint64
    ContextUsagePercent float32
    CurrentTaskID       string
    StartedAt     time.Time
    UpdatedAt     time.Time
    RuntimeAddr   string         // gRPC address of agent runtime
}
```

### Roles (7)

| # | Role | Lifetime | Capabilities | Example |
|---|------|----------|--------------|---------|
| 0 | **kernel** | Permanent, single instance | Everything | King (PID 1) |
| 1 | **daemon** | Permanent, auto-restart | Spawn, kill, manage tree, broadcast | Queen (per-VPS manager) |
| 2 | **agent** | Permanent, long-lived | Spawn, kill, network, file I/O | Leo (user-facing assistant) |
| 3 | **architect** | Ephemeral / sleep | File read/write, escalate | Design planner |
| 4 | **lead** | Long-lived, coordinates | Spawn, kill, delegate, budget | Team lead |
| 5 | **worker** | Long-lived, executor | Spawn (limited), network, file I/O | Code writer |
| 6 | **task** | Short-lived, atomic | File read, escalate only | Single unit of work |

### Cognitive Tiers (3)

| # | Tier | Default Model | Typical Role |
|---|------|---------------|--------------|
| 0 | **strategic** | opus | Kernel, top-level agents |
| 1 | **tactical** | sonnet | Leads, daemons, workers |
| 2 | **operational** | mini/haiku | Tasks, sub-workers |

**Constraint**: Child cognitive tier must be >= parent tier (numerically). A tactical parent cannot spawn a strategic child.

### Process States (6)

```
              spawn
                |
                v
  +-------+  assign  +--------+
  | idle  |--------->| running|
  +-------+          +--------+
      |                 |    |
      | sleep      block|    |complete/kill
      v                 v    v
  +--------+       +-------+ +--------+
  |sleeping|       |blocked| | zombie |--[reap]--> removed from registry
  +--------+       +-------+ +--------+
      |                |
      | wake      unblock
      v                v
  +-------+       +--------+
  | idle  |       | running|
  +-------+       +--------+
```

**Zombie lifecycle** (modeled after Linux):
- When a process dies (kill, crash, or task completion), it enters **zombie** state
- The kernel sends **SIGCHLD** to the parent, and the result is stored for collection
- The parent can call **`wait_child(pid)`** to collect the exit code and output
- The **supervisor** periodically scans for zombies older than `ZombieTimeout` (60s) and removes them from the registry via `Registry.Remove()`
- This prevents orphaned entries in the process table while giving parents time to collect results

### Spawn Validation

The spawner (`internal/process/spawner.go`) enforces:

1. **Parent alive**: Parent must not be in `dead` or `zombie` state
2. **Cognitive tier constraint**: `child.tier >= parent.tier` (lower tier number = higher capability)
3. **Max children**: Parent's `Limits.MaxChildren` not exceeded
4. **Role/tier combo**: Task role cannot have strategic tier
5. **Name required**: Every process must have a name
6. **User inheritance**: Non-kernel parents must pass their own user to children

### Process Tree Example

```
  PID 1  king (strategic, kernel)
    |
    +-- PID 2  queen@vps1 (tactical, daemon)
    |     |
    |     +-- PID 4  research-lead (tactical, lead)
    |     |     |
    |     |     +-- PID 5  researcher-0 (operational, task)
    |     |     +-- PID 6  researcher-1 (operational, task)
    |     |     +-- PID 7  researcher-2 (operational, task)
    |     |
    |     +-- PID 3  demo-worker (tactical, worker)
    |
    +-- PID 8  queen@vps2 (tactical, daemon)  [multi-VPS]
```

### Key Files

- `internal/process/types.go` — Process struct, roles, tiers, states
- `internal/process/registry.go` — Thread-safe process table (CRUD, tree traversal, NCA)
- `internal/process/spawner.go` — Spawn validation and registration
- `internal/process/signals.go` — Signal routing (SIGTERM, SIGKILL, SIGCHLD, SIGSTOP, SIGCONT)
- `internal/process/tree_ops.go` — KillBranch, Reparent, OrphanAdoption
- `internal/process/supervisor.go` — Daemon restart, task exit handling, zombie reaping
- `internal/process/lifecycle.go` — Sleep/wake, CompleteTask, WaitResult, CollapseBranch

---

## 3. IPC System

Inter-process communication follows hierarchical routing rules.

### Message Structure

```go
// internal/ipc/queue.go
type Message struct {
    ID          string
    FromPID     process.PID
    FromName    string
    ToPID       process.PID
    ToQueue     string          // named queue (alternative to direct PID)
    Type        string
    Priority    int             // 0=critical, 1=high, 2=normal, 3=low
    Payload     []byte
    RequiresAck bool
    TTL         time.Duration   // 0 = no expiry
    EnqueuedAt  time.Time
    ExpiresAt   time.Time
}
```

### Routing Rules

```
  Parent <--direct--> Child        (always allowed)
  Sibling --broker--> Sibling      (parent gets a copy)
  Cross-branch: via Nearest Common Ancestor (NCA)
  Task: can only send to its parent (escalation only)
```

**Routing diagram:**

```
         PID 1 (king)
         /          \
    PID 2            PID 8
    /    \
  PID 3  PID 4       Message PID 3 -> PID 4:
           |            1. Find relationship: siblings (same PPID=2)
         PID 5          2. Route: sibling (copy to parent PID 2)
                        3. Deliver to PID 4 inbox
                        4. Low-priority copy to PID 2 inbox

                      Message PID 5 -> PID 3:
                        1. NCA = PID 2
                        2. Route: ancestor
                        3. Deliver to PID 3 inbox
```

### Priority Queue with Aging

Messages are ordered by **effective priority**:

```
effective_priority = base_priority - age_seconds * aging_factor
```

Lower effective priority value = higher dispatch priority. Old messages gradually gain priority, preventing starvation. Default aging factor: 0.1 priority points/second.

The queue (`internal/ipc/queue.go`) uses `container/heap` with re-heapification on each pop to account for changing effective priorities. Expired messages (TTL exceeded) are silently dropped.

### Broker

The broker (`internal/ipc/broker.go`) provides:

- **Per-process inboxes**: Lazy-created `PriorityQueue` per PID
- **Named queues**: For pub/sub patterns (e.g., `Subscribe` RPC)
- **Route validation**: Checks sender/receiver exist, enforces task-only-to-parent rule
- **Sibling copy**: When siblings communicate, parent gets a low-priority copy for oversight
- **Relationship-based priority**: Priority adjusted based on parent/child/sibling relationship

### Shared Memory (Artifacts)

Artifacts are stored in-memory with visibility-based access control.

```go
// internal/ipc/shared_memory.go
type Artifact struct {
    ID          string
    Key         string
    Content     []byte
    ContentType string
    Visibility  Visibility      // private/user/subtree/global
    StoredByPID process.PID
    StoredAt    time.Time
}
```

**Visibility levels:**

| Level | Value | Access Rule |
|-------|-------|-------------|
| `private` | 0 | Only the storing process |
| `user` | 1 | All processes with same USER |
| `subtree` | 2 | Storer + all its descendants |
| `global` | 3 | Everyone |

**Operations**: Store (by key), Get (by key or ID), List (with prefix filter), Delete (storer or kernel only).

### Pipes

Bidirectional byte channels between parent and child (`internal/ipc/pipe.go`).

- Buffered channels (default 64 items)
- Non-blocking writes (error if full = backpressure)
- `PipeRegistry` manages creation/lookup/cleanup by parent-child pair

### EventBus

Pub/sub broadcast events (`internal/ipc/events.go`).

- Topics are strings: `"escalation"`, `"budget_exceeded"`, `"migration_completed"`
- Subscribe returns a buffered channel
- Publish is non-blocking: full subscriber channels drop the event
- Used for system-wide notifications (not agent-to-agent messaging)

### Key Files

- `internal/ipc/queue.go` — PriorityQueue with aging
- `internal/ipc/broker.go` — Message routing, inboxes, named queues
- `internal/ipc/shared_memory.go` — Artifact storage with visibility ACL
- `internal/ipc/pipe.go` — Bidirectional parent-child pipes
- `internal/ipc/events.go` — Pub/sub event bus
- `internal/ipc/relationship.go` — DetermineRelationship, ComputeEffectivePriority

---

## 4. Resource Management

### Token Budgets

Budgets flow down the process tree. Parent allocates to child from its own pool.

```
  King: 1M opus / 10M sonnet / 100M mini tokens
    |
    +-- Queen: allocated 500K sonnet from King
    |     |
    |     +-- Lead: allocated 100K sonnet from Queen
    |           |
    |           +-- Worker: allocated 20K mini from Lead
```

```go
// internal/resources/budget.go
type BudgetEntry struct {
    PID       process.PID
    Tier      ModelTier       // "opus", "sonnet", "mini"
    Allocated uint64          // total tokens allocated by parent
    Consumed  uint64          // tokens used so far
    Reserved  uint64          // reserved for children's allocations
}

// Remaining = Allocated - Consumed - Reserved
```

**Operations:**

| Method | Description |
|--------|-------------|
| `SetBudget(pid, tier, tokens)` | Set initial allocation (kernel/top-level) |
| `Allocate(parent, child, tier, tokens)` | Transfer tokens from parent to child |
| `Consume(pid, tier, tokens)` | Record usage (fails if budget exceeded) |
| `Release(child, tier)` | Return unused budget to parent on death |
| `BranchUsage(root, tier)` | Total consumed across entire subtree |

### Rate Limiter

Sliding window rate limiter (`internal/resources/rate_limiter.go`):
- Per-PID rate limits
- Configurable window size and max requests
- Automatic expiry of old entries

### Accounting

Usage tracking (`internal/resources/accounting.go`):
- Record token consumption by PID + tier
- Query usage by: user, VPS, process, tier
- Aggregated across subtrees

### CGroups

Collective resource limits for process groups (`internal/resources/cgroups.go`):

```go
type CGroup struct {
    Name           string
    RootPID        process.PID
    MaxTokensTotal uint64          // group-wide token limit
    MaxProcesses   int             // max concurrent processes
    TokensConsumed uint64
    ProcessCount   int
    members        map[process.PID]struct{}
}
```

- Auto-adds root + descendants on creation
- New children auto-join parent's cgroup
- `CheckSpawnAllowed`: validates process count limit
- `ConsumeTokens`: validates group-wide token limit

### Resource Flow Diagram

```
  King sets initial budgets
    |
    | Allocate(king, queen, sonnet, 500K)
    v
  Queen (500K sonnet allocated)
    |
    | Allocate(queen, lead, sonnet, 100K)
    | Queen.Reserved += 100K
    v
  Lead (100K sonnet allocated)
    |
    | Consume(lead, sonnet, 5K)
    | Lead.Consumed += 5K
    v
  Lead dies -> Release(lead, sonnet)
    |
    | Queen.Reserved -= 100K  (return allocation)
    | Unused = 100K - 5K = 95K  (returned to pool)
    v
  Queen now has 400K + 95K = 495K available
```

### Key Files

- `internal/resources/budget.go` — BudgetManager, allocate/consume/release
- `internal/resources/rate_limiter.go` — RateLimiter (sliding window)
- `internal/resources/accounting.go` — Accountant (usage by user/VPS/PID/tier)
- `internal/resources/cgroups.go` — CGroupManager (group limits)
- `internal/resources/limits.go` — LimitChecker (spawn/context/timeout validation)

---

## 5. Permissions

Three-layer permission system: Auth (identity), ACL (actions), Capabilities (fine-grained).

### Auth (Identity)

```go
// internal/permissions/auth.go
type Identity struct {
    PID  process.PID
    User string
    Role process.AgentRole
}
```

**Rules:**
- USER is inherited down the branch. All children work under parent's identity.
- Only kernel (PID 1) can assign a different user to a child.
- `ValidateInheritance(parentPID, childUser)` enforces this on spawn.
- `IsSameUser(pidA, pidB)` checks identity match for cross-process access.

### ACL (Actions)

Role-based access control for 7 system actions (`internal/permissions/acl.go`):

| Action | kernel | daemon | agent | architect | lead | worker | task |
|--------|--------|--------|-------|-----------|------|--------|------|
| spawn | Y | Y | Y | - | Y | Y | **N** |
| kill | Y | Y | - | - | Y | - | - |
| send_message | Y | Y | Y | - | Y | Y | Y* |
| read_artifact | Y | Y | Y | Y | Y | Y | Y |
| write_artifact | Y | Y | Y | Y | Y | Y | - |
| escalate | Y | Y | Y | Y | Y | Y | Y |
| read_process | Y | Y | Y | Y | Y | Y | - |

*Task can only send to its parent (enforced by broker).

- Rules checked with `ACL.Check(pid, action)` -- first match wins
- Custom rules can be added with `ACL.AddRule()`
- Cross-user access requires kernel mediation: `ACL.CheckCrossUser(requester, target)`

### Capabilities (Fine-Grained)

10 system capabilities, role-mapped with per-process overrides (`internal/permissions/capabilities.go`):

| Capability | Description | kernel | daemon | agent | architect | lead | worker | task |
|-----------|-------------|--------|--------|-------|-----------|------|--------|------|
| `spawn_children` | Spawn child processes | Y | Y | Y | - | Y | Y | - |
| `kill_processes` | Kill child processes | Y | Y | Y | - | Y | - | - |
| `manage_tree` | Reparent, migrate | Y | Y | - | - | - | - | - |
| `shell_exec` | Execute shell commands | Y | - | - | - | - | - | - |
| `network_access` | Make HTTP requests | Y | Y | Y | - | Y | Y | - |
| `file_write` | Write to filesystem | Y | - | Y | Y | Y | Y | - |
| `file_read` | Read from filesystem | Y | Y | Y | Y | Y | Y | Y |
| `budget_allocate` | Allocate budget to children | Y | Y | Y | - | Y | - | - |
| `escalate` | Escalate to parent | Y | Y | Y | Y | Y | Y | Y |
| `broadcast` | Publish events | Y | Y | - | - | - | - | - |

- `Grant(pid, cap)` / `Revoke(pid, cap)` for per-process overrides
- `ValidateTools(pid, tools)` checks tool capabilities against role (e.g., task cannot have shell_exec)

### Permission Check Flow (on SpawnChild)

```
  SpawnChild request arrives
    |
    +-- ACL.Check(parentPID, "spawn")         -- can this role spawn?
    +-- Caps.RequireCapability(parentPID,       -- does it have spawn_children cap?
    |       "spawn_children")
    +-- Auth.ValidateInheritance(parentPID,     -- user identity correct?
    |       childUser)
    +-- Caps.ValidateTools(childRole, tools)    -- child role allows these tools?
    +-- CGroups.CheckSpawnAllowed(parentPID)    -- cgroup process limit ok?
    +-- Spawner.Spawn(req)                      -- parent alive, cognitive tier, max_children, etc.
    |
    v
  Process created in registry
```

### Key Files

- `internal/permissions/auth.go` — AuthProvider (identity, inheritance)
- `internal/permissions/acl.go` — ACL (role-based action control)
- `internal/permissions/capabilities.go` — CapabilityChecker (10 capabilities, per-role + overrides)

---

## 6. Scheduling

### Task Priority

Tasks are prioritized by cognitive tier + role, with aging to prevent starvation.

```go
// internal/scheduler/priority.go
type TaskPriority struct {
    Base        int           // from tier + role
    EnqueuedAt  time.Time
    AgingFactor float64
}

// Effective() = Base + age_seconds * AgingFactor
// Higher effective = dispatched sooner
```

**Base priority computation:**

| Tier | Base | Role | Bonus | Combined Example |
|------|------|------|-------|-----------------|
| strategic | 100 | kernel | +50 | 150 |
| tactical | 50 | daemon | +40 | 90 |
| tactical | 50 | lead | +20 | 70 |
| operational | 10 | task | +5 | 15 |

### Task States

```
  submit       assign       start        finish
    |            |            |             |
    v            v            v             v
  pending --> assigned --> running --> completed
                                   \-> failed
                                   \-> cancelled
```

### Scheduler

The scheduler (`internal/scheduler/scheduler.go`) manages task dispatch:

| Method | Description |
|--------|-------------|
| `Submit(name, tier, role, submitter, payload)` | Add task to ready queue |
| `Assign(agentPID, agentTier, agentRole)` | Pop highest-priority compatible task |
| `Complete(taskID, success)` | Mark task completed/failed |
| `Cancel(taskID)` | Cancel pending/assigned task |
| `Resubmit(taskID)` | Return failed/cancelled task to queue |
| `TasksByState(state)` | List tasks by state |
| `TasksByAgent(pid)` | List tasks assigned to an agent |

**Compatibility check**: Agent's tier must be >= task requirement AND agent's role must be >= task requirement (lower number = higher capability).

### Cron Scheduler

Core-managed recurring actions (`internal/scheduler/cron.go`):

- **5-field cron expressions**: `minute hour day-of-month month day-of-week`
- Supports: `*`, specific values, `*/N` intervals, comma-separated values
- **Two actions**: `CronSpawn` (create new process) and `CronWake` (wake sleeping process)
- **Per-VPS**: Each entry is associated with a VPS node
- **Dedup**: Won't re-trigger within the same minute
- Queen holds the crontab (from spec)

### Lifecycle Manager

Dynamic process lifecycle (`internal/process/lifecycle.go`):

| Method | Description |
|--------|-------------|
| `Sleep(pid)` | Put agent into sleeping state (architect pattern) |
| `Wake(pid)` | Wake sleeping agent back to idle |
| `CompleteTask(pid, exitCode, output, error)` | Record result, mark **zombie**, SIGCHLD to parent |
| `WaitResult(pid)` | Collect result (like waitpid, removes after read) |
| `CollapseBranch(parentPID)` | Kill all children, collect results (lead completion) |
| `ActiveChildren(parentPID)` | Count non-dead, non-zombie children |

### Key Files

- `internal/scheduler/priority.go` — TaskPriority, ReadyQueue, base priority computation
- `internal/scheduler/scheduler.go` — Scheduler (submit, assign, complete, cancel)
- `internal/scheduler/cron.go` — CronScheduler (parse, match, per-VPS entries)
- `internal/process/lifecycle.go` — LifecycleManager (sleep/wake, complete, collapse)

---

## 7. Runtime

The runtime layer connects Go core to real Python agent processes.

### Architecture

```
  Go Core                           Python Agent
  +------------------+              +------------------+
  | RuntimeManager   |  os/exec     | runner.py        |
  |   StartRuntime() |----------->  |   (entry point)  |
  |                  |              |                  |
  |   Wait READY     |  stdout      |   Print READY    |
  |   <port>         |<-----------  |   <port>         |
  |                  |              |                  |
  |   grpc.Dial()    |  gRPC        |   grpc.aio       |
  |   Init()         |----------->  |   .server()      |
  |                  |              |                  |
  | Executor         |  Execute()   | HiveAgent        |
  |   ExecuteTask()  |<===========> |   handle_task()  |
  |   (bidi stream)  |  syscalls    |   ctx.spawn()    |
  |                  |  results     |   ctx.kill()     |
  +------------------+              +------------------+
```

### Manager (`internal/runtime/manager.go`)

The Manager handles agent process lifecycle:

1. **Virtual processes**: No `RuntimeImage` set -> register a map entry with `virtual://` address. No OS process spawned.
2. **Real processes**: `RuntimeImage` set (e.g., `"examples.demo_showcase:ResearchLead"`) -> full spawn cycle:

**Spawn lifecycle:**

```
  1. Build command:
     python -m hivekernel_sdk.runner --agent <image> --core <coreAddr>

  2. cmd.Start() -> OS process launched

  3. Wait for "READY <port>" on stdout (10s timeout)
     |
     v
  4. grpc.Dial("localhost:<port>")
     |
     v
  5. client.Init(pid, ppid, user, role, config)
     |
     v
  6. Agent is ready. RuntimeAddr = "localhost:<port>"
```

**Shutdown lifecycle:**

```
  1. client.Shutdown(reason, grace=5s)
     |
     v
  2. cmd.Wait() with 5s timeout
     |
     +-- exit gracefully -> done
     +-- timeout -> cmd.Process.Kill() -> reap
     |
     v
  3. conn.Close()
```

**Key methods:**

| Method | Description |
|--------|-------------|
| `StartRuntime(proc, rtType)` | Launch agent (virtual or real) |
| `StopRuntime(pid)` | Graceful shutdown + kill |
| `GetRuntime(pid)` | Get AgentRuntime by PID |
| `GetClient(pid)` | Get AgentServiceClient stub |
| `ListRuntimes()` | List all active runtimes |

### Executor (`internal/runtime/executor.go`)

Manages bidirectional Execute streams for task execution:

```
  Core (Executor)                    Agent (handle_task)
       |                                  |
       |-- TaskRequest -----------------> |
       |                                  |
       |                           ctx.spawn()
       |                                  |
       | <--- PROGRESS_SYSCALL (spawn) -- |
       |                                  |
       | HandleSyscall(spawn)             |
       |   -> king.SpawnChild()           |
       |                                  |
       |-- SyscallResult (child_pid) ---> |
       |                                  |
       |                           ctx.execute_on()
       |                                  |
       | <--- PROGRESS_SYSCALL (exec) --- |
       |                                  |
       | HandleSyscall(execute_on)        |
       |   -> executor.ExecuteTask()      |
       |      (recursive!)                |
       |                                  |
       |-- SyscallResult (result) ------> |
       |                                  |
       | <--- PROGRESS_COMPLETED -------- |
       |                                  |
       v                                  v
  Return TaskResult               Return TaskResult
```

**Syscall types dispatched by `KernelSyscallHandler`:**

| Syscall | Handler | Description |
|---------|---------|-------------|
| `spawn` | `king.SpawnChild()` | Spawn child process (real or virtual) |
| `kill` | `registry.SetState(zombie)` + `manager.StopRuntime()` + `NotifyParent()` | Kill child + descendants (zombie until reaped) |
| `send` | `broker.Route()` | Send IPC message (ACL + rate limit check) |
| `store` | `sharedMem.Store()` | Store artifact (ACL check) |
| `get_artifact` | `sharedMem.Get/GetByID()` | Retrieve artifact |
| `escalate` | Push to king's inbox | Report problem to parent |
| `log` | `log.Printf()` | Write log entry |
| `execute_on` | `executor.ExecuteTask()` (recursive) | Delegate task to child |
| `wait_child` | `lifecycle.WaitResult()` (poll) | Wait for child to exit (like waitpid) |

### Health Monitor (`internal/runtime/health.go`)

- Periodic heartbeat checks (configurable interval + timeout)
- Marks unresponsive agents as `zombie`
- Callback hook for custom handling (`OnUnhealthy`)
- Tracks last-seen timestamps per PID

### Key Files

- `internal/runtime/manager.go` — Manager (OS process spawn, READY protocol, gRPC dial, Init)
- `internal/runtime/executor.go` — Executor (bidi stream: TaskRequest -> syscalls -> TaskResult)
- `internal/runtime/health.go` — HealthMonitor (heartbeat timeout detection)
- `internal/kernel/syscall_handler.go` — KernelSyscallHandler (dispatch syscalls to subsystems)

---

## 8. Cluster

Multi-VPS support for distributed agent execution.

### Node Discovery (`internal/cluster/discovery.go`)

```go
type NodeInfo struct {
    ID            string       // "vps1", "vps2"
    Address       string       // gRPC address
    Cores         int
    MemoryMB      uint64
    MemoryUsedMB  uint64
    ProcessCount  int
    Status        NodeStatus   // online/overloaded/draining/offline
    LastHeartbeat time.Time
}
```

**Node statuses:** `online` -> `overloaded` (>90% memory) -> `draining` (migration in progress) -> `offline` (heartbeat timeout).

**Key methods:**

| Method | Description |
|--------|-------------|
| `Register(node)` | Add/update node |
| `FindLeastLoaded(exclude...)` | Pick migration target by LoadScore |
| `UpdateHealth(id, mem, disk, procs)` | Update metrics, auto-detect overload |
| `CheckStale(timeout)` | Mark nodes offline if heartbeat expired |

**LoadScore** = memory_usage_ratio + process_count * 0.01 (lower = better target).

### VPS Connector (`internal/cluster/connector.go`)

Connection pool with health tracking:

- `Connect(nodeID, address)` -> `ConnReady`
- `ForwardMessage(nodeID, msg)` -> tracks sent count, resets errors
- `RecordError(nodeID)` -> 3 consecutive errors -> `ConnUnhealthy`
- `RecordSuccess(nodeID)` -> reset errors, restore `ConnReady`

**Connection states:** `idle` -> `connecting` -> `ready` -> `unhealthy` -> `closed`

### Branch Migration (`internal/cluster/migration.go`)

Moves an entire process subtree from one VPS to another:

```
  1. PrepareMigration(rootPID, targetNode)
     - Validate: root exists, not kernel, target online, not same node
     - Snapshot root + all descendants
     - Create Migration record (state=pending)

  2. ExecuteMigration(migrationID)
     - Mark source node as draining
     - Update all processes: VPS = targetNode
     - If any fail: rollback all to sourceNode
     - Update node process counts
     - Mark migration completed

  3. RollbackMigration(migrationID)
     - Restore all processes to source VPS
     - Restore source node status
```

**Migration states:** `pending` -> `in_progress` -> `completed` / `failed` -> `rolled_back`

**Automatic migration trigger**: When king receives an `"vps_overloaded"` escalation, it finds the least-loaded node and migrates the escalating branch.

### Key Files

- `internal/cluster/discovery.go` — NodeRegistry (register, health, FindLeastLoaded)
- `internal/cluster/connector.go` — Connector (connection pool, message forwarding)
- `internal/cluster/migration.go` — MigrationManager (prepare, execute, rollback)

---

## 9. gRPC API Reference

Two gRPC services defined in `api/proto/`:

### AgentService (`agent.proto`)

Implemented by every Python agent runtime. Called by the Go core.

| RPC | Type | Description |
|-----|------|-------------|
| `Init(InitRequest) -> InitResponse` | Unary | Initialize agent with PID, role, config |
| `Shutdown(ShutdownRequest) -> ShutdownResponse` | Unary | Graceful shutdown, optional state snapshot |
| `Heartbeat(HeartbeatRequest) -> HeartbeatResponse` | Unary | Health check, returns state + metrics |
| `Execute(stream ExecuteInput) -> stream TaskProgress` | Bidi | Task execution with in-stream syscalls |
| `Interrupt(InterruptRequest) -> InterruptResponse` | Unary | Cancel current task (SIGINT) |
| `DeliverMessage(AgentMessage) -> MessageAck` | Unary | Deliver IPC message to agent |

### CoreService (`core.proto`)

Implemented by Go core. Called by agents and external clients.

| RPC | Type | Description |
|-----|------|-------------|
| `SpawnChild(SpawnRequest) -> SpawnResponse` | Unary | Spawn child process |
| `KillChild(KillRequest) -> KillResponse` | Unary | Kill child (optional recursive) |
| `GetProcessInfo(ProcessInfoRequest) -> ProcessInfo` | Unary | Get process details (pid=0 = self) |
| `ListChildren(ListChildrenRequest) -> ListChildrenResponse` | Unary | List children (optional recursive) |
| `SendMessage(SendMessageRequest) -> SendMessageResponse` | Unary | Send IPC message |
| `Subscribe(SubscribeRequest) -> stream AgentMessage` | Server stream | Subscribe to queue/inbox |
| `StoreArtifact(StoreArtifactRequest) -> StoreArtifactResponse` | Unary | Store artifact in shared memory |
| `GetArtifact(GetArtifactRequest) -> GetArtifactResponse` | Unary | Retrieve artifact by key/ID |
| `ListArtifacts(ListArtifactsRequest) -> ListArtifactsResponse` | Unary | List artifacts with prefix |
| `GetResourceUsage(ResourceUsageRequest) -> ResourceUsage` | Unary | Get token/child usage |
| `RequestResources(ResourceRequest) -> ResourceResponse` | Unary | Request additional tokens from parent |
| `Escalate(EscalateRequest) -> EscalateResponse` | Unary | Escalate problem to parent |
| `Log(LogRequest) -> LogResponse` | Unary | Write log entry |
| `ReportMetric(MetricRequest) -> MetricResponse` | Unary | Report metric (tokens_consumed triggers accounting) |
| `ExecuteTask(ExecuteTaskRequest) -> ExecuteTaskResponse` | Unary | Execute task on target agent (external entry point) |

### Caller Identification

Every CoreService RPC requires the caller's PID in gRPC metadata:

```
metadata key: "x-hivekernel-pid"
metadata value: "<pid>" (string representation of uint64)
```

Example in Python:
```python
metadata = [("x-hivekernel-pid", str(pid))]
stub.GetProcessInfo(request, metadata=metadata)
```

### Enums

| Enum | Values |
|------|--------|
| `AgentRole` | ROLE_KERNEL(0), ROLE_DAEMON(1), ROLE_AGENT(2), ROLE_ARCHITECT(3), ROLE_LEAD(4), ROLE_WORKER(5), ROLE_TASK(6) |
| `CognitiveTier` | COG_STRATEGIC(0), COG_TACTICAL(1), COG_OPERATIONAL(2) |
| `AgentState` | STATE_IDLE(0), STATE_RUNNING(1), STATE_BLOCKED(2), STATE_SLEEPING(3), STATE_DEAD(4), STATE_ZOMBIE(5) |
| `Priority` | PRIORITY_CRITICAL(0), PRIORITY_HIGH(1), PRIORITY_NORMAL(2), PRIORITY_LOW(3) |
| `ProgressType` | PROGRESS_UPDATE(0), PROGRESS_LOG(1), PROGRESS_COMPLETED(2), PROGRESS_FAILED(3), PROGRESS_NEEDS_INPUT(4), PROGRESS_SYSCALL(5) |
| `RuntimeType` | RUNTIME_PYTHON(0), RUNTIME_CLAW(1), RUNTIME_CUSTOM(2) |
| `ArtifactVisibility` | VIS_PRIVATE(0), VIS_USER(1), VIS_SUBTREE(2), VIS_GLOBAL(3) |
| `ShutdownReason` | SHUTDOWN_NORMAL(0), SHUTDOWN_PARENT_DIED(1), SHUTDOWN_BUDGET_EXCEEDED(2), SHUTDOWN_MIGRATION(3), SHUTDOWN_USER_REQUEST(4) |
| `EscalationSeverity` | ESC_INFO(0), ESC_WARNING(1), ESC_ERROR(2), ESC_CRITICAL(3) |
| `AckStatus` | ACK_ACCEPTED(0), ACK_QUEUED(1), ACK_REJECTED(2), ACK_ESCALATE(3) |

### Syscall Messages (in Execute stream)

8 syscall types embedded in `SystemCall.call` oneof:

| Syscall | Request | Response | Description |
|---------|---------|----------|-------------|
| `spawn` | `SpawnRequest` | `SpawnResponse` | Spawn child process |
| `kill` | `KillRequest` | `KillResponse` | Kill child |
| `send` | `SendMessageRequest` | `SendMessageResponse` | Send IPC message |
| `store` | `StoreArtifactRequest` | `StoreArtifactResponse` | Store artifact |
| `get_artifact` | `GetArtifactRequest` | `GetArtifactResponse` | Get artifact |
| `escalate` | `EscalateRequest` | `EscalateResponse` | Escalate to parent |
| `log` | `LogRequest` | `LogResponse` | Write log |
| `execute_on` | `ExecuteOnRequest` | `ExecuteOnResponse` | Delegate task to child |

Each syscall carries a unique `call_id` (UUID). The executor matches results by `call_id`.

---

## 10. Python SDK Reference

The SDK lives in `sdk/python/hivekernel_sdk/`.

### HiveAgent (`agent.py`)

Base class for all agents. Subclass and implement `handle_task()`.

```python
class HiveAgent:
    # Read-only properties
    pid: int          # assigned by kernel on Init
    ppid: int         # parent PID
    user: str         # inherited user identity
    role: str         # "kernel", "daemon", "lead", "task", etc.
    config: AgentConfig
    core: CoreClient  # direct access for on_init/on_shutdown

    # Override these methods:
    async def on_init(self, config: AgentConfig) -> None: ...
    async def handle_task(self, task: Task, ctx: SyscallContext) -> TaskResult: ...
    async def on_message(self, message: Message) -> MessageAck: ...
    async def on_shutdown(self, reason: str) -> bytes | None: ...
```

### SyscallContext (`syscall.py`)

Passed to `handle_task(task, ctx)`. Performs kernel operations via the Execute bidi stream.

| Method | Returns | Description |
|--------|---------|-------------|
| `ctx.spawn(name, role, cognitive_tier, ...)` | `int` (child PID) | Spawn child agent |
| `ctx.kill(pid, recursive=True)` | `list[int]` (killed PIDs) | Kill child agent |
| `ctx.send(to_pid, type, payload, ...)` | `str` (message_id) | Send IPC message |
| `ctx.store_artifact(key, content, content_type, visibility)` | `str` (artifact_id) | Store artifact |
| `ctx.get_artifact(key="", artifact_id="")` | `GetArtifactResponse` | Retrieve artifact |
| `ctx.escalate(issue, severity, auto_propagate)` | `str` (response) | Escalate to parent |
| `ctx.log(level, message, **fields)` | None | Write log entry |
| `ctx.execute_on(pid, description, params, timeout)` | `TaskResult` | Delegate task to child |
| `ctx.wait_child(pid, timeout_seconds=60)` | `dict` | Wait for child to exit (like waitpid) |
| `ctx.report_progress(message, percent)` | None | Send progress update (not a syscall) |

**spawn() parameters:**

```python
pid = await ctx.spawn(
    name="worker-0",              # required
    role="task",                  # kernel/daemon/agent/architect/lead/worker/task
    cognitive_tier="operational", # strategic/tactical/operational
    system_prompt="",             # LLM system prompt
    model="",                     # empty = auto from tier
    tools=None,                   # list of tool names
    initial_task="",
    limits=None,                  # dict for ResourceLimits
    runtime_image="mod:Class",    # Python class to run (triggers real spawn)
    runtime_type="python",        # python/claw/custom
)
```

### CoreClient (`client.py`)

Direct gRPC client for CoreService. Used in `on_init`/`on_shutdown` or by external scripts.

```python
class CoreClient:
    def __init__(self, channel, pid): ...

    # Process management
    async def spawn_child(name, role, cognitive_tier, ...) -> int
    async def kill_child(pid, recursive=True) -> list[int]
    async def get_process_info(pid=0) -> ProcessInfo
    async def list_children(recursive=False) -> list[ProcessInfo]

    # IPC
    async def send_message(to_pid, type, payload, ...) -> str

    # Artifacts
    async def store_artifact(key, content, content_type, visibility) -> str
    async def get_artifact(key="", artifact_id="") -> GetArtifactResponse
    async def list_artifacts(prefix="") -> list[ArtifactMeta]

    # Resources
    async def get_resource_usage() -> ResourceUsage
    async def request_resources(resource_type, amount) -> dict

    # Other
    async def escalate(issue, severity, auto_propagate) -> str
    async def log(level, message, **fields)
    async def report_metric(name, value, **labels)
```

### Types (`types.py`)

```python
@dataclass
class AgentConfig:
    name: str = ""
    system_prompt: str = ""
    model: str = ""
    metadata: dict[str, str] = field(default_factory=dict)

@dataclass
class Task:
    task_id: str = ""
    description: str = ""
    params: dict[str, str] = field(default_factory=dict)
    priority: str = "normal"
    timeout_seconds: int = 0
    context: bytes = b""
    parent_task_id: str = ""

@dataclass
class TaskResult:
    exit_code: int = 0
    output: str = ""
    artifacts: dict[str, str] = field(default_factory=dict)
    metadata: dict[str, str] = field(default_factory=dict)

@dataclass
class Message:
    message_id: str = ""
    from_pid: int = 0
    from_name: str = ""
    type: str = ""
    payload: bytes = b""
    timestamp: int = 0
    requires_ack: bool = False

@dataclass
class ResourceUsage:
    tokens_consumed: int = 0
    tokens_remaining: int = 0
    children_active: int = 0
    children_max: int = 0
    context_usage_percent: float = 0.0
    uptime_seconds: int = 0
```

### Runner (`runner.py`)

Entry point for agent processes spawned by the kernel:

```
python -m hivekernel_sdk.runner --agent my_module:MyClass --core localhost:50051
```

Flow:
1. Parse `--agent` as `module_path:ClassName`
2. `importlib.import_module(module_path)` -> `getattr(mod, ClassName)`
3. Create agent instance
4. Start `grpc.aio.server()` on `[::]:0` (random port)
5. Print `READY <port>` to stdout (signal to Go)
6. `await server.wait_for_termination()`

**PYTHONPATH** must include `sdk/python` for spawned agents to find modules.

### Key Files

- `sdk/python/hivekernel_sdk/agent.py` — HiveAgent base class
- `sdk/python/hivekernel_sdk/syscall.py` — SyscallContext (9 syscalls)
- `sdk/python/hivekernel_sdk/client.py` — CoreClient (async gRPC wrapper)
- `sdk/python/hivekernel_sdk/runner.py` — Runner entry point
- `sdk/python/hivekernel_sdk/types.py` — AgentConfig, Task, TaskResult, Message, etc.
