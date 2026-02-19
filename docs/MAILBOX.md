# Mailbox System

> Every agent has a mailbox. All interaction is messages.

## 1. Core Concept

The mailbox is the central interaction model in HiveKernel. Every agent has a priority inbox managed by the Broker. All work arrives as messages:

- **Agent-to-agent**: message -> inbox -> `handle_message()` -> reply
- **Cron tasks**: kernel sends message -> inbox -> agent processes -> reply
- **WebUI/external**: dashboard sends message -> inbox -> agent processes -> reply visible in dashboard
- **Syscalls**: agents use `self.core` (CoreClient) for kernel operations during message handling

The Execute bidi stream remains available for backwards compatibility but is no longer the recommended path for new agents.

## 2. Message Types

| Type | Direction | Purpose | Reply Expected |
|------|-----------|---------|----------------|
| `task_request` | any -> agent | "Do this work" (from cron, webui, another agent) | Yes (`task_response`) |
| `task_response` | agent -> requester | "Here's the result" (carries `reply_to`) | No |
| `cron_task` | kernel (PID 1) -> agent | Periodic instruction from cron scheduler | Yes (`cron_result`) |
| `cron_result` | agent -> kernel | Reply to `cron_task` | No |
| `notification` | any -> agent | Informational, no reply expected | No |
| `escalation` | child -> parent | Problem report flowing upward | No |

## 3. Message Lifecycle

```
  Sender                     Broker                      Receiver
    |                          |                            |
    |-- SendMessage() -------->|                            |
    |                          |-- Route + validate ------->|
    |                          |   (EventMessageSent)       |
    |                          |                            |
    |                          |-- Push to inbox ---------->|
    |                          |-- OnMessage callback ----->|
    |                          |   (EventMessageDelivered)  |
    |                          |                            |
    |                          |                    handle_message()
    |                          |                            |
    |<---- reply (optional) ---|<-- SendMessage(reply_to) --|
```

**States**: sent -> delivered -> (optional reply)

- **sent**: message routed through broker, in target's inbox
- **delivered**: push delivery succeeded (DeliverMessage RPC completed)
- **replied**: a reply message with matching `reply_to` was sent

## 4. Trace Propagation

Every message is automatically annotated with trace context from `EventLog.traceLookup`:

- `trace_id`: identifies the end-to-end operation
- `trace_span`: identifies the specific step within the trace

Traces propagate when messages cross agent boundaries. The dashboard Traces panel shows the full flow.

## 5. Interaction Patterns

### Request-Response (`send_and_wait`)

```python
# Agent A sends a task to Agent B and waits for reply
reply = await self.send_and_wait(ctx, to_pid=b_pid, type="task_request",
                                  payload=b'{"task": "analyze data"}')
result = reply.payload
```

### Fire-and-Forget

```python
# Send notification, don't wait
await self.core.send_message(to_pid=target, type="notification",
                              payload=b"FYI: backup complete")
```

### Reply Helper

```python
# Inside handle_message(), reply to the sender
async def handle_message(self, message):
    result = do_work(message.payload)
    await self.reply_to(message, payload=result, type="task_response")
    return MessageAck(status=MessageAck.ACK_ACCEPTED)
```

### Cron

Kernel sends `cron_task` message on schedule. Agent handles it in `handle_message()`:

```python
async def handle_message(self, message):
    if message.type == "cron_task":
        result = await self.run_maintenance()
        await self.reply_to(message, payload=result.encode(), type="cron_result")
    return MessageAck(status=MessageAck.ACK_ACCEPTED)
```

## 6. Syscalls During Message Handling

Agents use `self.core` (CoreClient) for kernel operations during `handle_message()`. No Execute stream needed:

```python
async def handle_message(self, message):
    # Spawn a child
    child_pid = await self.core.spawn_child(name="worker", role="task", ...)

    # Send a message to another agent
    await self.core.send_message(to_pid=sibling, type="task_request", payload=data)

    # Store an artifact
    await self.core.store_artifact(key="report", content=report_bytes, ...)

    # Log
    await self.core.log("info", "Task completed")

    return MessageAck(status=MessageAck.ACK_ACCEPTED)
```

## 7. Execute Stream (Legacy)

The bidirectional Execute stream (`handle_task` + `SyscallContext`) is still functional:

- Task arrives via Execute stream
- Agent performs syscalls through the stream context (`ctx.spawn()`, `ctx.kill()`, etc.)
- Result flows back through the stream

This path is **not recommended** for new agents. Use the mailbox model instead:
- Tasks arrive as messages in `handle_message()`
- Syscalls go through `self.core` (CoreClient, direct gRPC calls)
- Replies sent via `self.core.send_message(reply_to=...)`

The Execute stream remains for backwards compatibility with existing agents.

## 8. Inbox Inspection

The kernel provides inbox inspection for debugging and dashboard use:

- `Broker.ListInbox(pid)` -- peek at all messages in a process's inbox
- `Broker.InboxLen(pid)` -- count of messages in inbox
- `CoreService.ListInbox` RPC -- gRPC endpoint for inbox inspection
- Dashboard Inbox panel -- visual inbox viewer with send/receive tabs

## 9. Inbox Flush on Agent Ready

When an agent starts and completes Init(), any messages that arrived in its inbox before it was ready are flushed and re-delivered via the OnMessage callback. This ensures no messages are lost during agent startup.
