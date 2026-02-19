# Agent-Kernel Interaction Architecture

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

Agents interact with the kernel through three distinct patterns. Each has
different semantics, blocking behavior, and use cases.

### Pattern A: Direct Invocation (Execute bidi stream)

**Synchronous task delegation.** Caller blocks until target returns result.

```
Caller Agent                 Kernel (Executor)           Target Agent
    |                            |                           |
    | ctx.execute_on(pid, task)  |                           |
    |--------------------------->|                           |
    |  [caller blocks on Future] |-- Execute stream open --->|
    |                            |-- TaskRequest ----------->|
    |                            |                           |-- handle_task(task, ctx)
    |                            |                           |     ctx.spawn(...)
    |                            |<-- PROGRESS_SYSCALL ------|     [awaits Future]
    |                            |-- SyscallResult --------->|     [Future resolved]
    |                            |                           |     return TaskResult
    |                            |<-- PROGRESS_COMPLETED ----|
    |<-- SyscallResult(result) --|                           |
    |  [caller resumes]          |                           |
```

**Mechanism:**
- `ctx.execute_on(pid, desc)` -- via SyscallContext in Execute bidi stream
- `core.execute_task(pid, desc)` -- via CoreClient direct RPC to CoreService

Both end up calling `Executor.ExecuteTask()` on the Go side, which opens a
new Execute bidi stream to the target agent.

**Blocking:** Yes -- caller waits for TaskResult.

**Entry point on target:** `handle_task(task, ctx) -> TaskResult`

**Who uses it and how:**

| Caller | Target | How | Code |
|--------|--------|-----|------|
| Queen | Worker | Simple task: spawn, execute, kill | `core.execute_task(worker_pid, desc)` |
| Queen | Lead (Orchestrator) | Complex task: reuse/spawn lead, delegate | `core.execute_task(lead_pid, desc)` |
| Queen | Architect | Get strategic plan | `core.execute_task(arch_pid, desc)` |
| Orchestrator | Workers | Subtasks within groups, sequentially | `ctx.execute_on(wpid, subtask)` |
| Assistant | Coder | Code generation (synchronous) | `ctx.execute_on(coder_pid, request)` |
| Dashboard | Any agent | User-initiated task | CoreService.ExecuteTask RPC |

**Note:** Queen uses `core.execute_task()` (CoreClient RPC) even inside
`handle_task()` rather than `ctx.execute_on()` (SyscallContext). This is
intentional -- it lets the same `_core_handle_*` methods work from both
`handle_task()` (Execute stream entry) and `handle_message()` (IPC entry).

---

### Pattern B: Async Mailbox (IPC Messages via DeliverMessage)

**Fire-and-forget messaging.** Sender does not block. Optional reply
correlation via `reply_to` field.

```
Sender Agent              Kernel (Broker)          Receiver Agent
    |                          |                        |
    | ctx.send(to=B, payload)  |                        |
    |------------------------->|                        |
    | [sender continues]       |-- Route(msg) --------->|
    |                          |                        |-- DeliverMessage RPC
    |                          |                        |-- on_message(msg)
    |                          |                        |     handle_message(msg)
    |                          |                        |     reply_to(msg, result)
    |                          |<-- send(reply) --------|
    |<-- DeliverMessage -------|                        |
    |-- on_message (resolve)   |                        |
```

**Mechanism:**
- `ctx.send(to_pid, type, payload)` -- via SyscallContext during Execute stream
- `core.send_message(to_pid, type, payload)` -- via CoreClient direct RPC

Both route through the kernel's IPC Broker, which delivers to the target
agent's `DeliverMessage` gRPC method.

**Blocking:** No. Three sub-patterns:

1. **Fire-and-forget:** `send_fire_and_forget(ctx, pid, type, payload)` --
   send, don't wait, collect results later from mailbox
2. **Send-and-wait:** `send_and_wait(ctx, pid, type, payload, timeout)` --
   send, block on Future, Future resolved when correlated reply arrives via
   `on_message()` matching `reply_to` field
3. **Drain mailbox:** `drain_pending_results()` -- collect all stashed
   `task_response` messages that arrived asynchronously

**Entry point on target:** `on_message(msg) -> handle_message(msg) -> MessageAck`

**Reply correlation:** Sender generates `request_id`, sets it as `reply_to`
on the message. Receiver includes the same `reply_to` in their reply.
Sender's `on_message()` matches `reply_to` against `_pending_requests` dict
and resolves the corresponding Future.

**Who uses it and how:**

| Sender | Receiver | Pattern | Code |
|--------|----------|---------|------|
| Assistant | Queen | Fire-and-forget delegation | `send_fire_and_forget(ctx, queen_pid, "task_request", payload)` |
| Queen | Sender | Reply with result | `core.send_message(to=from_pid, reply_to=msg.reply_to)` |
| Assistant | (self) | Drain mailbox on next handle_task | `self.drain_pending_results()` |
| Cron engine | Any agent | Cron trigger | kernel sends `cron_task` via DeliverMessage |
| Any agent | Any agent | Peer-to-peer messaging | `ctx.send()` or `core.send_message()` |

**Real example -- Assistant delegates to Queen:**

```python
# Assistant._handle_delegation() -- sends async task to Queen
request_id = await self.send_fire_and_forget(
    ctx=ctx,
    to_pid=queen_pid,
    type="task_request",
    payload=json.dumps({"task": "research quantum computing"}).encode(),
)
# Track for later correlation
self._active_delegations[request_id] = {"target": "queen", "task": ...}
# Returns immediately: "[Delegated to queen -- results will appear in next message]"
```

```python
# Queen.handle_message() -- receives task_request, processes, replies
async def handle_message(self, message):
    if message.type == "task_request":
        asyncio.create_task(self._process_message_task(message))
        return MessageAck(status=ACK_QUEUED)

async def _process_message_task(self, message):
    # ... execute task ...
    await self.core.send_message(
        to_pid=message.from_pid,
        type="task_response",
        payload=result_payload,
        reply_to=message.reply_to,  # correlation ID
    )
```

```python
# Assistant.handle_task() -- on NEXT call, drains mailbox
completed_results = self.drain_pending_results()
for msg in completed_results:
    delegation = self._active_delegations.pop(msg.reply_to, None)
    # ... include result in response to user ...
```

---

### Pattern C: Kernel Operations (CoreClient direct RPC)

**Direct agent-to-kernel calls.** Not agent-to-agent. Synchronous, fast
(no LLM involved).

```
Agent                    CoreService (Go kernel)
    |                           |
    | core.spawn_child(...)     |-- Registry.Spawn()
    | core.store_artifact(...)  |-- SharedMemory.Store()
    | core.add_cron(...)        |-- Cron.Add()
    | core.log(...)             |-- EventLog.Emit()
    | core.get_process_info()   |-- Registry.Get()
    | core.list_children()      |-- Registry.GetChildren()
    | core.list_artifacts()     |-- SharedMemory.List()
    | core.get_resource_usage() |-- Budgets.Usage()
```

**Mechanism:** Direct gRPC RPC to CoreService stub. Authenticated by
`x-hivekernel-pid` metadata header.

**Blocking:** Yes, but instant (kernel operations, no network/LLM).

**Who uses it:**
- **All agents:** `core.log()`, `core.store_artifact()`, `core.get_artifact()`
- **Queen:** `core.spawn_child()`, `core.kill_child()`, `core.execute_task()`,
  `core.get_process_info()`
- **Assistant:** `core.add_cron()`, `core.send_message()`
- **ToolAgent built-in tools:** `spawn_child`, `execute_task`, `send_message`,
  `store_artifact`, `get_artifact` etc.

---

## 3. Comparison Table

| Aspect | A: Direct Invocation | B: Async Mailbox | C: Kernel Ops |
|--------|:---:|:---:|:---:|
| **Semantics** | Request/Response | Fire & (optionally) forget | Request/Response |
| **Blocking** | Yes | No (or with send_and_wait) | Yes (instant) |
| **Go-side mechanism** | Executor bidi stream | Broker -> DeliverMessage RPC | CoreService RPC |
| **Python-side trigger** | `ctx.execute_on()` / `core.execute_task()` | `ctx.send()` / `core.send_message()` | `core.*()` |
| **Entry point on target** | `handle_task()` | `handle_message()` | N/A (kernel) |
| **Return value** | `TaskResult` | `MessageAck` (+ optional reply) | RPC response |
| **Supports syscalls** | Yes (nested via stream) | No | N/A |
| **Use case** | Task delegation | Async inter-agent messaging | Infrastructure ops |

## 4. SyscallContext vs CoreClient

Both provide the same 10 syscalls, but through different channels:

| Syscall | SyscallContext (bidi stream) | CoreClient (direct RPC) |
|---------|:---:|:---:|
| `spawn` | `ctx.spawn()` | `core.spawn_child()` |
| `kill` | `ctx.kill()` | `core.kill_child()` |
| `send` | `ctx.send()` | `core.send_message()` |
| `execute_on` | `ctx.execute_on()` | `core.execute_task()` |
| `store_artifact` | `ctx.store_artifact()` | `core.store_artifact()` |
| `get_artifact` | `ctx.get_artifact()` | `core.get_artifact()` |
| `escalate` | `ctx.escalate()` | `core.escalate()` |
| `log` | `ctx.log()` | `core.log()` |
| `wait_child` | `ctx.wait_child()` | `core.wait_child()` |
| `list_siblings` | `ctx.list_siblings()` | `core.list_siblings()` |

**SyscallContext** (`ctx`) -- available only inside `handle_task()`. Syscalls
go through the Execute bidi stream: agent puts SystemCall on output queue,
kernel's Executor dispatches to SyscallHandler, result comes back through the
same stream.

**CoreClient** (`self.core`) -- available everywhere (on_init, handle_task,
handle_message, background tasks). Syscalls go as direct gRPC RPCs to
CoreService. Authenticated by PID metadata.

**ToolContext** (Plan 013) -- wrapper that delegates to SyscallContext if
available, falls back to CoreClient. Used by ToolAgent's built-in tools so
they work in both `handle_task()` and `handle_message()` contexts.

## 5. Agent Class Hierarchy

```
HiveAgent                     lifecycle, Execute bidi stream, DeliverMessage
  +-- LLMAgent                + LLMClient (OpenRouter), ask(), chat()
        +-- QueenAgent           task dispatcher daemon, 3-tier routing
        +-- AssistantAgent       24/7 chat daemon, async delegation
        +-- CoderAgent           code generation daemon
        +-- OrchestratorAgent    task decomposition, parallel groups
        +-- WorkerAgent          stateful worker, accumulated context
        +-- ArchitectAgent       strategic planner
        +-- MaidAgent            LLM health monitoring
        +-- GitHubMonitorAgent   cron-triggered repo watcher
        +-- ToolAgent (Plan 013) + AgentLoop + ToolRegistry + AgentMemory
```

**HiveAgent** -- base class. Implements AgentService gRPC interface via inner
Servicer. Handles Init, Shutdown, Heartbeat, Execute (bidi stream with 3
concurrent asyncio tasks: reader, writer, runner), DeliverMessage. Provides
async messaging (send_and_wait, drain_pending_results).

**LLMAgent** -- adds OpenRouter LLMClient. Loads API key from env/.env.
Provides `ask(prompt)` and `chat(messages)` shortcuts.

**ToolAgent** -- adds iterative LLM + tool execution engine (AgentLoop),
tool registry with 9 built-in syscall tools, and artifact-backed persistent
memory (AgentMemory). New agents should subclass ToolAgent and override
`get_tools()` to add custom tools.

## 6. IPC Message Routing

Messages are routed by the kernel's IPC Broker based on process tree
relationships:

- **Parent <-> child:** direct delivery
- **Siblings:** through broker (same parent)
- **Cross-branch:** through nearest common ancestor

The Broker uses a priority queue with aging. Messages have TTL, priority
(critical/high/normal/low), and optional `requires_ack` flag.

## 7. Kernel-Side Dispatch

```
Agent process sends syscall via Execute stream
    |
    v
Executor.ExecuteTask()          (runtime/executor.go)
    |-- stream.Recv() -> TaskProgress(SYSCALL)
    |-- for each SystemCall:
    |       SyscallHandler.HandleSyscall()   (kernel/syscall_handler.go)
    |           |-- handleSpawn()    -> King.SpawnChild() -> Registry + Manager
    |           |-- handleKill()     -> Registry.SetState + Manager.StopRuntime
    |           |-- handleSend()     -> ACL check + RateLimit + Broker.Route()
    |           |-- handleStore()    -> ACL check + SharedMemory.Store()
    |           |-- handleExecuteOn() -> Executor.ExecuteTask() [recursive!]
    |           |-- handleWaitChild() -> poll Registry until zombie/dead
    |           |-- ...
    |       stream.Send(SyscallResult)
    |-- stream.Recv() -> TaskProgress(COMPLETED)
    v
Return TaskResult to caller
```

`execute_on` is recursive: the SyscallHandler calls Executor.ExecuteTask
on the target agent, which opens a new Execute stream. This allows
multi-level delegation (Queen -> Lead -> Worker) with each level having
its own active Execute stream.

## 8. Key Files Reference

### Go kernel
| File | Responsibility |
|------|---------------|
| `internal/runtime/manager.go` | OS process spawn, READY handshake, gRPC dial, Init RPC |
| `internal/runtime/executor.go` | Execute bidi stream management, syscall dispatch loop |
| `internal/kernel/syscall_handler.go` | Dispatches 10 syscalls to King's subsystems |
| `internal/kernel/grpc_core.go` | CoreService gRPC server (direct RPC API) |
| `internal/kernel/king.go` | Central kernel: registry, broker, shared memory, budgets |
| `internal/runtime/health.go` | Heartbeat monitoring (10s interval, 3 failures = kill) |
| `api/proto/agent.proto` | AgentService definition + all message types |
| `api/proto/core.proto` | CoreService definition |

### Python SDK
| File | Responsibility |
|------|---------------|
| `runner.py` | Entry point: load class, start gRPC server, print READY |
| `agent.py` | HiveAgent base: lifecycle, Execute stream, DeliverMessage |
| `llm_agent.py` | LLMAgent: adds LLMClient (OpenRouter) |
| `syscall.py` | SyscallContext: 10 syscalls via Execute bidi stream |
| `client.py` | CoreClient: 10 syscalls + extras via direct gRPC RPC |
| `tool_agent.py` | ToolAgent: agent loop + tools + memory |
| `loop.py` | AgentLoop: iterative LLM + tool execution engine |
| `memory.py` | AgentMemory: artifact-backed persistent state |
| `tools.py` | Tool protocol, ToolContext, ToolRegistry |
| `builtin_tools.py` | 9 built-in syscall tools for ToolAgent |
| `llm.py` | LLMClient: async OpenRouter HTTP client |
