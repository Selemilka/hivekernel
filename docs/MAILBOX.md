# Mailbox System

> Every agent has a mailbox. All work arrives as messages. Parents delegate
> to children via execute_task.

## 1. Core Concept

HiveKernel has two interaction models, each for a different purpose:

### Receiving tasks: Mailbox (handle_message)

Every agent has a priority inbox managed by the Broker. All incoming work
arrives as messages:

- **Agent-to-agent**: message -> inbox -> `handle_message()` -> reply
- **Cron tasks**: kernel sends `cron_task` -> inbox -> agent processes -> reply
- **WebUI/external**: dashboard sends `task_request` -> inbox -> agent processes -> reply

The priority queue in the inbox orders messages by priority with aging.
This IS the scheduling -- no separate scheduler needed.

### Delegating to children: execute_task (handle_task)

When a parent wants a child to do work and needs the result:

```python
result = await self.core.execute_task(child_pid, "research quantum computing")
# result is TaskResult -- synchronous, one line, no correlation logic
```

The child receives the task in `handle_task(task, ctx) -> TaskResult`.
This is ideal for atomic workers: spawn, execute, get result, kill.

### When to use which

| I want to... | Use | Entry on target |
|--------------|-----|-----------------|
| Send a task to a peer/daemon | `core.send_message()` (mailbox) | `handle_message()` |
| Receive tasks from anyone | Implement `handle_message()` | -- |
| Delegate to my child and wait | `core.execute_task()` | `handle_task()` |
| Fire-and-forget to anyone | `send_fire_and_forget()` | `handle_message()` |

**Rule of thumb**: receive via mailbox, delegate via execute_task.

### What NOT to do

**Do not implement the same logic in both handle_task() and handle_message().**
Queen historically had this problem -- duplicate business logic in two entry
points. The fix (plan 012): Queen receives ALL tasks via handle_message(),
delegates to children via core.execute_task().

### Kernel-level Scheduler (unused)

`internal/scheduler/scheduler.go` implements a pull-based task queue for flat
worker pools. In the hierarchical tree model, parents always route work to
children -- making a centralized scheduler redundant. The code exists with
tests but is not wired to any production path. See section 8 for rationale.

## 2. Message Types

| Type | Direction | Purpose | Reply Expected |
|------|-----------|---------|----------------|
| `task_request` | any -> agent | "Do this work" | Yes (`task_response`) |
| `task_response` | agent -> requester | "Here's the result" (carries `reply_to`) | No |
| `cron_task` | kernel (PID 1) -> agent | Periodic instruction from cron scheduler | Optional (`cron_result`) |
| `cron_result` | agent -> kernel | Reply to `cron_task` | No |
| `notification` | any -> agent | Informational, no reply expected | No |
| `escalation` | child -> parent | Problem report flowing upward | No |

## 3. Message Lifecycle

```
  Sender                     Broker                      Receiver
    |                          |                            |
    |-- send_message() ------->|                            |
    |   (via CoreClient RPC    |-- Route + validate ------->|
    |    or send syscall)      |   (relationship priority)  |
    |                          |                            |
    |                          |-- Push to inbox (queue) -->|
    |                          |-- OnMessage callback ----->|
    |                          |   (push delivery)          |
    |                          |                            |
    |                          |                    DeliverMessage RPC
    |                          |                    on_message() called
    |                          |                    handle_message() called
    |                          |                            |
    |<---- reply (optional) ---|<-- send_message(reply_to) -|
```

**Key**: message goes to inbox AND is immediately pushed via DeliverMessage RPC.
The inbox retains messages only if the agent isn't ready yet (flushed on Init).

## 4. Priority and Ordering

Messages in the inbox are ordered by effective priority:

```
effective_priority = base_priority - age_seconds * aging_factor
```

Priority is adjusted based on relationship:
- **Parent -> child**: HIGH priority (parent's instructions are important)
- **Sibling -> sibling**: NORMAL priority
- **Child -> parent**: varies (escalations get HIGH)

Old messages gain priority over time (aging), preventing starvation.
Default aging factor: 0.1 points/second.

## 5. Routing Rules

```
Parent <--direct--> Child        (always allowed)
Sibling --broker--> Sibling      (parent gets a low-priority copy)
Cross-branch: via Nearest Common Ancestor
Task role: can only send to its parent
```

## 6. Interaction Patterns

### Request-Response (send_and_wait)

```python
# Agent A sends to Agent B and waits for correlated reply
reply = await self.send_and_wait(ctx, to_pid=b_pid, type="task_request",
                                  payload=b'{"task": "analyze data"}')
result = reply.payload
```

Internally: generates request_id, sends message with reply_to=request_id,
registers Future in `_pending_requests`. When reply arrives via on_message()
with matching reply_to, Future is resolved.

### Fire-and-Forget

```python
# Send, don't wait. Collect results later.
request_id = await self.send_fire_and_forget(ctx, to_pid=target,
                                              type="task_request", payload=data)
# Later:
results = self.drain_pending_results()  # collects stashed task_response messages
```

### Reply Helper

```python
# Inside handle_message(), reply to the sender
async def handle_message(self, message):
    result = await do_work(message.payload)
    await self.reply_to(message, payload=result, type="task_response")
    return MessageAck(status=MessageAck.ACK_ACCEPTED)
```

### Cron

Kernel sends `cron_task` message on schedule. Agent handles in handle_message():

```python
async def handle_message(self, message):
    if message.type == "cron_task":
        result = await self.run_maintenance()
        await self.reply_to(message, payload=result.encode(), type="cron_result")
    return MessageAck(status=MessageAck.ACK_ACCEPTED)
```

## 7. Syscalls During Message Handling

Agents use `self.core` (CoreClient) for kernel operations. No Execute stream needed:

```python
async def handle_message(self, message):
    # Spawn a child
    child_pid = await self.core.spawn_child(name="worker", role="task", ...)

    # Delegate work (opens Execute stream to child -- transparent to caller)
    result = await self.core.execute_task(child_pid, description="...")

    # Send a message to another agent
    await self.core.send_message(to_pid=sibling, type="task_request", payload=data)

    # Store an artifact
    await self.core.store_artifact(key="report", content=report_bytes, ...)

    return MessageAck(status=MessageAck.ACK_ACCEPTED)
```

## 8. Who Routes Tasks (Agent vs Kernel)

**Agents always decide routing.** The kernel only delivers.

| Decision | Who Decides | How |
|----------|-------------|-----|
| "This task is complex, spawn a lead" | Queen | `_assess_complexity()` -> `_core_handle_complex()` |
| "Split into 3 subtasks for workers" | Orchestrator/Lead | `_decompose_task()` -> spawn workers -> execute_on |
| "This is a coding task, send to Coder" | Assistant | `send_fire_and_forget(coder_pid, ...)` |
| "New commits found, tell Queen" | GitMonitor | `core.send_message(queen_pid, "escalation", ...)` |
| "Time to check health" | Kernel (cron) | Sends `cron_task` to configured PID |

The kernel's Broker validates routing (can this sender reach this receiver?)
and adjusts priority (parent messages go first). But it never decides WHO
gets the message -- that's always the sender's choice.

**Why no centralized scheduler?**

In a flat job queue (like Celery), workers are anonymous and interchangeable.
A scheduler picks "whoever is free". In HiveKernel's tree:

- Parents know their children's capabilities (they configured them)
- Queen manages her own lead pool (acquire idle lead, or spawn new one)
- Orchestrator distributes to its own worker pool
- Each parent IS the scheduler for its subtree

A kernel-level scheduler would duplicate what parents already do, and worse --
it would need to understand agent capabilities, which violates the separation
between kernel (infrastructure) and agents (business logic).

## 9. execute_task: Synchronous Delegation

`core.execute_task(pid, description)` is the parent->child delegation mechanism.
Internally it opens an Execute bidi stream to the child, sends TaskRequest,
handles syscalls, and returns TaskResult. But from the caller's perspective
it's a simple RPC: call, wait, get result.

**Ideal for atomic workers** (parallel research, scraping, code generation):

```python
# Orchestrator: spawn 10 workers, run in parallel, collect results
workers = [await self.core.spawn_child(name=f"w-{i}", role="task", ...) for i in range(10)]
results = await asyncio.gather(*[
    self.core.execute_task(w, f"research subtopic {i}")
    for i, w in enumerate(workers)
])
# results is a list of TaskResult -- no correlation needed
for w in workers:
    await self.core.kill_child(w)
```

**Worker side is trivial:**

```python
async def handle_task(self, task, ctx) -> TaskResult:
    result = await self.ask(task.description)
    return TaskResult(exit_code=0, output=result)
```

Compare with the mailbox equivalent (more boilerplate, manual correlation):

```python
# Caller: send 10 messages, wait for 10 replies, correlate by reply_to
# Worker: parse message, do work, send reply with reply_to, return ACK
```

**execute_task is not legacy** -- it's the right tool for synchronous
parent->child delegation. What IS legacy is receiving tasks through BOTH
handle_task and handle_message with duplicated logic (Queen's historical
problem, fixed by plan 012).

## 10. Inbox Inspection

The kernel provides inbox inspection for debugging and dashboard:

- `Broker.ListInbox(pid)` -- peek at all messages in a process's inbox
- `Broker.InboxLen(pid)` -- count of messages in inbox
- `CoreService.ListInbox` RPC -- gRPC endpoint for inbox inspection
- Dashboard Messages panel -- shows sent/delivered message history
- Dashboard Inbox view -- shows current unprocessed messages per PID

## 11. Inbox Flush on Agent Ready

When an agent completes Init(), any messages that arrived before it was ready
are flushed and re-delivered via the OnMessage callback. No messages are lost
during startup.
