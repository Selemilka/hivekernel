# Agent-Kernel Interaction

How Python SDK agents communicate with the Go kernel.

## 1. Agent Lifecycle

```
Go Kernel (Manager)                          Python Agent (Runner)
    |                                             |
    |-- exec("python -m hivekernel_sdk.runner")-->|
    |                                             |-- load class, create instance
    |                                             |-- connect CoreClient -> kernel
    |                                             |-- start gRPC server on :0
    |<-------- stdout: "READY 50123" -------------|
    |                                             |
    |-- grpc.Dial("localhost:50123") ------------>|
    |-- Init(pid, ppid, role, config, limits) --->|-- on_init(config)
    |<-------- InitResponse(ready=true) ----------|
    |                                             |
    |   [agent running, heartbeat every 10s]      |
    |                                             |
    |-- Shutdown(reason, grace=5s) -------------->|-- on_shutdown()
    |<-------- ShutdownResponse ------------------|-- server.stop()
```

Key files:
- Go: `internal/runtime/manager.go` (StartRuntime -> launchAndConnect)
- Python: `sdk/python/hivekernel_sdk/runner.py` (entry point), `agent.py` (_make_servicer)

## 2. Three Interaction Patterns

### Pattern A: Mailbox (receiving tasks, async messaging)

**Asynchronous message delivery.** How tasks arrive at agents.

```
Sender Agent              Kernel (Broker)          Receiver Agent
    |                          |                        |
    | core.send_message(B, ..) |                        |
    |------------------------->|                        |
    | [sender continues]       |-- Route + validate --->|
    |                          |-- priority + aging      |
    |                          |-- push to inbox ------->|
    |                          |-- OnMessage callback -->|
    |                          |                        |-- DeliverMessage RPC
    |                          |                        |-- on_message(msg)
    |                          |                        |     handle_message(msg)
    |                          |                        |     reply_to(msg, result)
    |                          |<-- send(reply) --------|
    |<-- DeliverMessage -------|                        |
    |-- on_message(resolve)    |                        |
```

**Sub-patterns:**

| Pattern | Blocking | Description |
|---------|----------|-------------|
| Fire-and-forget | No | `send_fire_and_forget(ctx, pid, type, payload)` |
| Send-and-wait | Yes (timeout) | `send_and_wait(ctx, pid, type, payload)` |
| Drain mailbox | No | `drain_pending_results()` -- collect stashed replies |

**Entry point on receiver:** `handle_message(msg) -> MessageAck`

**Who uses it:**

| Sender | Receiver | Pattern | Purpose |
|--------|----------|---------|---------|
| Assistant | Queen | Fire-and-forget | Delegate complex tasks |
| Queen | Assistant | Reply (reply_to) | Return task results |
| Kernel cron | Any agent | Push delivery | Trigger periodic work |
| Dashboard | Any agent | Send message | User-initiated tasks |
| Any agent | Any agent | Direct message | Peer communication |

### Pattern B: Task delegation (synchronous parent->child)

**Synchronous execution.** Parent delegates task to child and waits for result.

```
Parent Agent             Kernel (Executor)           Child Agent
    |                         |                          |
    | core.execute_task(pid)  |                          |
    |------------------------>|-- Execute stream open --->|
    | [parent blocks]         |-- TaskRequest ---------->|
    |                         |                          |-- handle_task(task, ctx)
    |                         |                          |     do work...
    |                         |                          |     return TaskResult
    |                         |<-- PROGRESS_COMPLETED ---|
    |<-- result --------------|                          |
    | [parent resumes]        |                          |
```

**Ideal for atomic workers** -- spawn, execute, get result, kill:

```python
workers = [await self.core.spawn_child(name=f"w-{i}", role="task", ...) for i in range(10)]
results = await asyncio.gather(*[
    self.core.execute_task(w, f"research subtopic {i}")
    for i, w in enumerate(workers)
])
# results is list[TaskResult] -- no correlation, no reply_to, no boilerplate
```

**Entry point on child:** `handle_task(task, ctx) -> TaskResult`

**Who uses it:**

| Parent | Child | Purpose |
|--------|-------|---------|
| Queen | Worker | Simple task delegation |
| Queen | Lead (Orchestrator) | Complex task decomposition |
| Orchestrator | Workers | Parallel subtask execution |
| Dashboard | Any agent | User-initiated execution |

### Pattern C: Kernel Operations (infrastructure)

**Direct agent-to-kernel calls.** Not agent-to-agent.

```
Agent                    CoreService (Go kernel)
    |                           |
    | core.spawn_child(...)     |-- Registry.Spawn()
    | core.kill_child(...)      |-- Registry.SetState + Manager.Stop
    | core.store_artifact(...)  |-- SharedMemory.Store()
    | core.get_artifact(...)    |-- SharedMemory.Get()
    | core.add_cron(...)        |-- Cron.Add()
    | core.log(...)             |-- EventLog.Emit()
    | core.get_process_info()   |-- Registry.Get()
    | core.list_children()      |-- Registry.GetChildren()
```

## 3. Comparison

| Aspect | A: Mailbox | B: Task Delegation | C: Kernel Ops |
|--------|:---:|:---:|:---:|
| **Purpose** | Receive tasks, peer messaging | Parent->child work delegation | Infrastructure operations |
| **Blocking** | No (or send_and_wait) | Yes (waits for result) | Yes (instant) |
| **Go mechanism** | Broker -> DeliverMessage | Executor bidi stream | CoreService RPC |
| **Python caller** | `core.send_message()` | `core.execute_task()` | `core.*()` |
| **Entry on target** | `handle_message()` | `handle_task()` | N/A (kernel) |
| **Return** | `MessageAck` + opt reply | `TaskResult` | RPC response |
| **Best for** | Daemons, peers, cron | Atomic workers, parallel subtasks | spawn, kill, store, log |

## 4. Task Routing: Who Decides?

**Agents always decide routing.** The kernel only delivers.

The process tree IS the routing topology. Parents know their children's
capabilities because they configured and spawned them.

```
User sends task via WebUI
    |
    v
Queen (daemon) receives task_request message in inbox
    |
    +-- assess_complexity(task)
    |
    +-- Simple? -> spawn worker, execute, kill, return result
    |
    +-- Complex? -> acquire idle lead from pool (or spawn new)
    |              -> execute_task(lead_pid, task)
    |              -> lead decomposes into subtasks
    |              -> lead spawns workers, distributes, synthesizes
    |
    +-- Architect? -> spawn architect, get plan
                   -> execute plan via lead
```

Each parent is the scheduler for its subtree:
- **Queen** manages her lead pool (reuse idle leads, spawn new ones)
- **Orchestrator/Lead** distributes subtasks across worker pool
- **Assistant** routes to Queen (complex) or Coder (code tasks)

A kernel-level centralized scheduler would duplicate this logic and violate
the separation between kernel (infrastructure) and agents (business logic).
See [MAILBOX.md](MAILBOX.md) section 8 for detailed rationale.

## 5. The Two Entry Points

Every agent has two entry points. They serve different purposes:

| Entry | Triggered by | Purpose | Returns |
|-------|-------------|---------|---------|
| `handle_message(msg)` | Broker delivers message | Receive tasks, notifications, replies | `MessageAck` |
| `handle_task(task, ctx)` | `core.execute_task()` from parent | Execute delegated work | `TaskResult` |

**Rule**: receive via mailbox (handle_message), delegate via execute_task (handle_task).

**Anti-pattern**: implementing the same business logic in BOTH entry points.
If an agent needs to accept tasks from both peers (mailbox) and parents
(execute_task), it should have ONE internal method called from both handlers.

For **long-lived daemons** (Queen, Assistant): implement `handle_message()`.
For **short-lived workers** (task role, atomic work): implement `handle_task()`.
For **both** (leads, orchestrators): implement both, but with shared logic.

## 6. SyscallContext vs CoreClient

Both provide the same syscalls through different channels:

| Syscall | SyscallContext (`ctx`) | CoreClient (`self.core`) |
|---------|:---:|:---:|
| spawn | `ctx.spawn()` | `core.spawn_child()` |
| kill | `ctx.kill()` | `core.kill_child()` |
| send | `ctx.send()` | `core.send_message()` |
| execute_on | `ctx.execute_on()` | `core.execute_task()` |
| store_artifact | `ctx.store_artifact()` | `core.store_artifact()` |
| get_artifact | `ctx.get_artifact()` | `core.get_artifact()` |
| escalate | `ctx.escalate()` | `core.escalate()` |
| log | `ctx.log()` | `core.log()` |
| wait_child | `ctx.wait_child()` | -- |
| list_siblings | `ctx.list_siblings()` | -- |

**SyscallContext** -- only inside `handle_task()`. Goes through Execute stream.
**CoreClient** -- available everywhere. Goes as direct gRPC RPC.

For new agents, prefer CoreClient. SyscallContext exists for legacy compatibility.

## 7. Agent Class Hierarchy

```
HiveAgent                     lifecycle, Execute stream, DeliverMessage, mailbox helpers
  +-- LLMAgent                + LLMClient (OpenRouter), ask(), chat()
        +-- QueenAgent           task dispatcher daemon, 3-tier routing
        +-- AssistantAgent       24/7 chat daemon, async delegation
        +-- CoderAgent           code generation daemon
        +-- OrchestratorAgent    task decomposition, parallel groups
        +-- WorkerAgent          stateful worker, accumulated context
        +-- ArchitectAgent       strategic planner
        +-- MaidAgent            LLM health monitoring
        +-- GitHubMonitorAgent   cron-triggered repo watcher
        +-- ToolAgent            + AgentLoop + ToolRegistry + AgentMemory
```

**ToolAgent** is the universal agent class (Plan 014). New agents should
use ToolAgent with config-driven behavior (system prompt from .md file,
tool set from config) rather than creating new subclasses.

## 8. Key Files

### Go kernel
| File | Responsibility |
|------|---------------|
| `internal/runtime/manager.go` | OS process spawn, READY handshake, gRPC dial, Init RPC |
| `internal/runtime/executor.go` | Execute bidi stream, syscall dispatch loop |
| `internal/kernel/syscall_handler.go` | Dispatches syscalls to King's subsystems |
| `internal/kernel/grpc_core.go` | CoreService gRPC server (direct RPC API) |
| `internal/kernel/king.go` | Central kernel: registry, broker, shared memory, budgets |
| `internal/ipc/broker.go` | Message routing, inboxes, priority queues |
| `internal/runtime/health.go` | Heartbeat monitoring |

### Python SDK
| File | Responsibility |
|------|---------------|
| `runner.py` | Entry point: load class, start gRPC server, print READY |
| `agent.py` | HiveAgent base: lifecycle, Execute stream, DeliverMessage, mailbox helpers |
| `client.py` | CoreClient: syscalls + extras via direct gRPC RPC |
| `tool_agent.py` | ToolAgent: agent loop + tools + memory, config-driven |
| `loop.py` | AgentLoop: iterative LLM + tool execution engine |
| `memory.py` | AgentMemory: artifact-backed persistent state |
| `tools.py` | Tool protocol, ToolContext, ToolRegistry |
| `builtin_tools.py` | 11 built-in syscall tools for ToolAgent |
