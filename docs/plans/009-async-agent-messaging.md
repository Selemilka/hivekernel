# Plan 009: Async Agent Messaging (Request-Response over IPC)

## Problem

Agents (assistant, queen, coder, researcher) are all siblings -- children of King (PID 1).
The `execute_on()` syscall only works parent->child. So assistant **cannot** delegate
tasks to queen, coder, or any sibling. The only inter-sibling primitive is `send()`,
but it's fire-and-forget with no request-response pattern.

Currently assistant is limited to:
- Answering questions via LLM
- Scheduling cron jobs
- `DELEGATE_CODE:` marker hack (which also fails -- coder is not assistant's child)

## Existing Infrastructure (already implemented)

| Component | Location | Status |
|-----------|----------|--------|
| Per-agent inbox (PriorityQueue) | `ipc/broker.go` | Working |
| Priority aging: `eff = base - age * 0.1` | `ipc/queue.go` | Working |
| Relationship-based boost (parent=HIGH, sibling=NORMAL) | `ipc/priority.go` | Working |
| Sibling routing through broker | `ipc/broker.go` Route() | Working |
| `send()` syscall -> broker.Route() -> inbox | `syscall_handler.go` | Working |
| `Subscribe()` gRPC stream (pull from inbox) | `grpc_core.go` | Working |
| `DeliverMessage` RPC (kernel pushes to agent) | `agent.py` Servicer | Working |
| `on_message()` hook in HiveAgent | `agent.py` base class | Stub (returns ACK) |
| `AckStatus` enum (ACCEPTED, QUEUED, REJECTED, ESCALATE) | `agent.proto` | Defined |
| Message TTL, RequiresAck | `ipc/queue.go` Message struct | Working |

The queue, routing, and delivery are all done. Only the "glue" is missing.

## Design: Async Mailbox Pattern

Key principle from user: **assistant sends request and does NOT block waiting**.
While waiting for queen's response, assistant can handle other tasks.
Responses arrive in the agent's message queue alongside other messages.

```
    Assistant                    Kernel Broker                  Queen
        |                            |                           |
        |-- send(queen, task_req) -->|                           |
        |   returns msg_id           |-- Route to queen inbox -->|
        |                            |                           |
        | (free to handle            |   DeliverMessage -------->|
        |  other tasks)              |                           |
        |                            |                           | processes...
        |                            |                           | spawns workers...
        |                            |<-- send(assistant, resp) -|
        |                            |    reply_to=msg_id        |
        |<-- DeliverMessage ---------|                           |
        | on_message() resolves      |                           |
        | pending request tracker    |                           |
```

## Implementation Plan

### Phase 1: Proto changes (~5 lines)

**File: `api/proto/agent.proto`**

Add `reply_to` field to `SendMessageRequest` and `AgentMessage`:

```protobuf
message SendMessageRequest {
    ...existing fields...
    string reply_to = 8;  // correlation: "this is a reply to message X"
}

message AgentMessage {
    ...existing fields...
    string reply_to = 10;  // correlation: "this is a reply to message X"
}
```

Regenerate proto (Go + Python), fix Python imports.

### Phase 2: Propagate reply_to through kernel (~20 lines Go)

**File: `internal/ipc/queue.go`**
- Add `ReplyTo string` field to `Message` struct

**File: `internal/kernel/syscall_handler.go`**
- In `handleSend()`: copy `req.ReplyTo` -> `msg.ReplyTo`

**File: `internal/kernel/grpc_core.go`**
- In `Subscribe()`: copy `msg.ReplyTo` -> `pbMsg.ReplyTo`

**File: `internal/runtime/manager.go` (or wherever DeliverMessage is called)**
- When delivering message to agent via `DeliverMessage` RPC, include `reply_to`

### Phase 3: list_siblings syscall (~40 lines Go + Python)

New syscall so agents can discover siblings without dashboard passing PIDs.

**File: `api/proto/agent.proto`**
```protobuf
message ListSiblingsRequest {}
message ListSiblingsResponse {
    repeated SiblingInfo siblings = 1;
}
message SiblingInfo {
    uint64 pid = 1;
    string name = 2;
    string role = 3;
    string state = 4;
}
```

Add to `SystemCall` oneof and `SyscallResult` oneof.

**File: `internal/kernel/syscall_handler.go`**
- `handleListSiblings()`: get caller's PPID, list parent's children, exclude caller

**File: `sdk/python/hivekernel_sdk/syscall.py`**
- `async def list_siblings() -> list[dict]`

### Phase 4: RequestTracker in Python HiveAgent (~40 lines Python)

**File: `sdk/python/hivekernel_sdk/agent.py`**

Add to `HiveAgent.__init__`:
```python
self._pending_requests: dict[str, asyncio.Future] = {}
```

Update `on_message()` base implementation:
```python
async def on_message(self, message: Message) -> MessageAck:
    # If this is a reply to a pending request, resolve the future.
    if message.reply_to and message.reply_to in self._pending_requests:
        fut = self._pending_requests.pop(message.reply_to)
        if not fut.done():
            fut.set_result(message)
        return MessageAck(status=MessageAck.ACK_ACCEPTED)
    # Otherwise delegate to subclass handler.
    return await self.handle_message(message)

async def handle_message(self, message: Message) -> MessageAck:
    """Override in subclass to handle non-reply messages."""
    return MessageAck(status=MessageAck.ACK_ACCEPTED)
```

Add helper method:
```python
async def send_and_wait(self, ctx: SyscallContext, to_pid: int,
                        type: str, payload: bytes,
                        timeout: float = 60.0) -> Message:
    """Send message and wait for reply. Non-blocking for other tasks."""
    msg_id = await ctx.send(to_pid=to_pid, type=type, payload=payload,
                            requires_ack=True)
    fut = asyncio.get_event_loop().create_future()
    self._pending_requests[msg_id] = fut
    try:
        return await asyncio.wait_for(fut, timeout=timeout)
    except asyncio.TimeoutError:
        self._pending_requests.pop(msg_id, None)
        raise
```

Note: `send_and_wait` blocks the current task but NOT the agent. Other tasks
and messages are processed concurrently via the gRPC servicer. For truly
non-blocking fire-and-forget, agent just calls `ctx.send()` directly and
picks up the reply later in `handle_message()`.

### Phase 5: Queen handles incoming messages (~50 lines Python)

**File: `sdk/python/hivekernel_sdk/queen.py`**

Override `handle_message()`:
```python
async def handle_message(self, message: Message) -> MessageAck:
    if message.type == "task_request":
        # Parse request payload.
        request = json.loads(message.payload)
        description = request.get("description", "")

        # Process asynchronously (don't block the message handler).
        asyncio.create_task(self._process_message_task(message, description))
        return MessageAck(status=MessageAck.ACK_QUEUED)

    return MessageAck(status=MessageAck.ACK_ACCEPTED)

async def _process_message_task(self, message: Message, description: str):
    """Process a task request received via IPC message."""
    # Use the same complexity-based routing as handle_task.
    # Queen spawns workers/lead as children, so execute_on works fine.
    result = await self._route_task(description, self._last_ctx)

    # Send response back to the requester.
    response_payload = json.dumps({
        "exit_code": result.exit_code,
        "output": result.output,
    }).encode()
    await self._last_ctx.send(
        to_pid=message.from_pid,
        type="task_response",
        payload=response_payload,
        reply_to=message.message_id,
    )
```

### Phase 6: Assistant uses messaging for delegation (~60 lines Python)

**File: `sdk/python/hivekernel_sdk/assistant.py`**

Update SYSTEM_PROMPT to include delegation via messaging:
```python
SYSTEM_PROMPT = """You are HiveKernel Assistant, a helpful AI running inside a process tree OS.

Available actions:
- Answer questions directly
- Schedule recurring tasks: SCHEDULE: {"name": "...", "cron": "...", ...}
- Delegate to a sibling agent: DELEGATE: {"to": "<name>", "task": "..."}
  Use this when a task requires specialized processing (research, coding, etc.)

Your siblings (other agents) will be listed at the start of each conversation.
When delegating, the response will arrive asynchronously -- wait for it.
"""
```

In `handle_task()`:
1. Call `ctx.list_siblings()` to discover available agents
2. Include sibling list in LLM context
3. Parse `DELEGATE:` blocks from LLM response
4. Use `self.send_and_wait(ctx, sibling_pid, "task_request", payload)` to delegate
5. Include the response in the final output

### Phase 7: Kernel-side message delivery trigger (~30 lines Go)

Currently messages sit in inbox until agent calls `Subscribe()`. But agents
don't have a long-running Subscribe stream -- they handle tasks via Execute.

**Need**: When broker.Route() deposits a message, kernel calls `DeliverMessage`
RPC on the target agent's gRPC server (push model).

**File: `internal/ipc/broker.go`**
- Add `OnMessage func(pid PID, msg *Message)` callback to Broker
- Call it after successful Route()

**File: `internal/kernel/king.go` or `grpc_core.go`**
- Register callback: when message arrives for PID, call agent's DeliverMessage RPC
- Use runtime manager to get agent's gRPC address

This ensures messages are delivered immediately, not waiting for Subscribe poll.

### Phase 8: WebUI -- Message Flow Visualization

#### 8a. Backend: message history endpoint

**File: `sdk/python/dashboard/app.py`**

New in-memory store (like task_history):
```python
message_log: list[dict] = []  # {from_pid, to_pid, type, reply_to, timestamp, payload_preview}
```

New endpoint:
```python
@app.get("/api/messages")
async def api_messages(pid: int = 0, limit: int = 50):
    # Filter by pid (sent or received), return recent messages
```

Hook into kernel event stream or add a new event type for IPC messages.

#### 8b. Frontend: Message panel in node details

**File: `sdk/python/dashboard/static/index.html`**

New panel "Messages" between Activity Log and Task History:
- Shows sent/received messages for selected node
- Color-coded: outgoing (blue), incoming (green), replies (gray)
- Shows payload preview, from/to names, timestamp

**File: `sdk/python/dashboard/static/app.js`**
- `loadMessages(pid)` -- fetch `/api/messages?pid=N`
- Render in messages panel

#### 8c. Frontend: delegation flow visualization

**File: `sdk/python/dashboard/static/app.js`**

When a message is sent between agents, animate:
- Highlight the link between sender and receiver in the D3 tree
- Flash the edge briefly to show message flow direction
- Optional: show a small "envelope" icon traveling along the edge

#### 8d. Cron page: add target agent dropdown from siblings

Already done (uses tree nodes). No changes needed.

---

## File Summary

| # | File | Changes |
|---|------|---------|
| 1 | `api/proto/agent.proto` | reply_to in SendMessageRequest + AgentMessage, ListSiblings messages |
| 2 | `api/proto/hivepb/*` | Regenerated Go proto |
| 3 | `sdk/python/hivekernel_sdk/*_pb2*.py` | Regenerated Python proto |
| 4 | `internal/ipc/queue.go` | ReplyTo field in Message |
| 5 | `internal/ipc/broker.go` | OnMessage callback |
| 6 | `internal/kernel/syscall_handler.go` | handleSend: reply_to, handleListSiblings |
| 7 | `internal/kernel/grpc_core.go` | Subscribe: reply_to, DeliverMessage push |
| 8 | `sdk/python/hivekernel_sdk/agent.py` | RequestTracker, send_and_wait, handle_message |
| 9 | `sdk/python/hivekernel_sdk/syscall.py` | list_siblings(), send() reply_to param |
| 10 | `sdk/python/hivekernel_sdk/queen.py` | handle_message: process task_request, send reply |
| 11 | `sdk/python/hivekernel_sdk/assistant.py` | Updated SYSTEM_PROMPT, delegation via DELEGATE: |
| 12 | `sdk/python/dashboard/app.py` | /api/messages endpoint, message_log |
| 13 | `sdk/python/dashboard/static/index.html` | Messages panel |
| 14 | `sdk/python/dashboard/static/app.js` | loadMessages(), message rendering |
| 15 | `sdk/python/dashboard/static/style.css` | Message panel styles |

## Execution Order

1. Proto changes + regenerate (Phase 1)
2. Go kernel: reply_to propagation + list_siblings + delivery push (Phases 2, 3, 7)
3. Python SDK: RequestTracker + send_and_wait + list_siblings (Phase 4)
4. Queen: handle_message (Phase 5)
5. Assistant: delegation via messaging (Phase 6)
6. WebUI: message visualization (Phase 8)
7. Build + tests

## Verification

1. `go build` + `go test ./internal/...`
2. Start kernel with startup-showcase.json
3. Open dashboard, chat with assistant: "ask queen to research X"
4. Assistant sends IPC to queen, queen processes, sends reply
5. Activity Log shows message flow on both nodes
6. Messages panel shows sent/received with correlation
