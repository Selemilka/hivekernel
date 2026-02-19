# Plan 012: Migrate Queen to Mailbox-First Pattern

## Context

HiveKernel has two task delegation mechanisms:

1. **Execute stream** (`execute_on`) -- synchronous bidi gRPC stream. Parent blocks
   until child returns `TaskResult`. Child uses `SyscallContext` for syscalls.
2. **Mailbox** (inbox messages) -- async IPC. Message arrives in inbox,
   `handle_message()` fires, agent uses `self.core` (CoreClient) for syscalls,
   replies via `send_message(reply_to=...)`.

`MAILBOX.md` declares Execute stream "legacy, not recommended", yet all agents
(Queen, Orchestrator, Worker) use it exclusively. Queen has **duplicate code paths**:
`handle_task` (full-featured, via execute_on) and `handle_message` (partial, via IPC)
with different feature sets.

**Goal:** Unify Queen to a single mailbox-first code path. External callers (dashboard,
cron, siblings) all use the same message-based entry. Delete duplicated code.

**Approach:** Hybrid -- Queen enters via `handle_message`, delegates to children via
`self.core.execute_task()`. Children (Orchestrator, Worker) keep `handle_task` as-is.
This gives the biggest win (one code path, non-blocking entry) with smallest blast
radius.

## Phase 0: Fill CoreClient Gaps

CoreClient is missing `list_siblings` and `wait_child` (only available via
SyscallContext). Also need `send_and_wait` that works without SyscallContext.

### 0.1 Add RPCs to `api/proto/core.proto`

Add to `CoreService`:
```proto
rpc ListSiblings(hivekernel.agent.ListSiblingsRequest) returns (hivekernel.agent.ListSiblingsResponse);
rpc WaitChild(hivekernel.agent.WaitChildRequest) returns (hivekernel.agent.WaitChildResponse);
```

Message types already exist in `agent.proto` (lines 402-428). No new messages needed.

### 0.2 Implement RPCs in `internal/kernel/grpc_core.go`

**ListSiblings:** Extract caller PID via `callerPID(ctx)`, look up process, get
parent's children, filter out self. Logic identical to `handleListSiblings` in
`syscall_handler.go:432-465`.

**WaitChild:** Extract caller PID, validate target is caller's child, poll registry
until zombie/dead or timeout. Logic identical to `handleWaitChild` in
`syscall_handler.go:467-530`.

### 0.3 Regenerate proto bindings

```bash
# Go
"C:/protoc/bin/protoc.exe" --proto_path=api/proto \
  --go_out=api/proto/hivepb --go_opt=paths=source_relative \
  --go-grpc_out=api/proto/hivepb --go-grpc_opt=paths=source_relative \
  api/proto/agent.proto api/proto/core.proto

# Python
python -m grpc_tools.protoc --proto_path=api/proto \
  --python_out=sdk/python/hivekernel_sdk \
  --grpc_python_out=sdk/python/hivekernel_sdk \
  api/proto/agent.proto api/proto/core.proto
# Fix imports: `import agent_pb2` -> `from . import agent_pb2`
```

### 0.4 Add methods to `sdk/python/hivekernel_sdk/client.py`

Add `list_siblings()` -> `list[dict]` and `wait_child(pid, timeout)` -> `dict`.

### 0.5 Add `send_and_wait_core` to `sdk/python/hivekernel_sdk/agent.py`

Current `send_and_wait(ctx, ...)` requires SyscallContext. Add a new method that
uses `self._core.send_message()` instead of `ctx.send()`. Same future-based
correlation via `self._pending_requests`.

### 0.6 Tests

- Go: test `ListSiblings` and `WaitChild` RPCs in `grpc_core_test.go`
- Python: test CoreClient wrappers + `send_and_wait_core` in `test_agent.py`

**Files:** `api/proto/core.proto`, `internal/kernel/grpc_core.go`,
`sdk/python/hivekernel_sdk/client.py`, `sdk/python/hivekernel_sdk/agent.py`

---

## Phase 1: Extract Core-Compatible Queen Methods

Separate Queen's business logic from the transport layer. No behavioral changes.

### 1.1 Create `_core_handle_simple`, `_core_handle_complex`, `_core_handle_architect`

New private methods in `queen.py` that use `self.core` instead of `ctx`:
- `ctx.spawn(...)` -> `self.core.spawn_child(...)`
- `ctx.execute_on(pid, ...)` -> `self.core.execute_task(pid, ...)`
- `ctx.kill(pid)` -> `self.core.kill_child(pid)`
- `ctx.log(...)` -> `self.core.log(...)`
- `ctx.store_artifact(...)` -> `self.core.store_artifact(...)`
- `ctx.report_progress(...)` -> `self.core.log(...)` (progress as log messages)

All three methods return `dict` with `exit_code`, `output`, `metadata` keys
(same shape as `core.execute_task` return value).

### 1.2 Update `_acquire_lead` to support both paths

Add optional `ctx=None` parameter. When `ctx is None`, use `self.core.spawn_child`.
Health check already uses `self.core.get_process_info` (no change needed).

### 1.3 Tests

Test `_core_handle_*` methods with mocked `self.core`.

**Files:** `sdk/python/hivekernel_sdk/queen.py`, `sdk/python/tests/test_queen.py`

---

## Phase 2: Unify Queen's handle_message

Replace the duplicated `_ipc_handle_simple` / `_ipc_handle_complex` with the new
`_core_*` methods. Add missing features to the IPC path.

### 2.1 Rewrite `_process_message_task`

Use `_core_handle_*` methods. Add what the IPC path was missing:
- Architect path (was missing entirely)
- Lead reuse (IPC path killed leads after each task)
- Task history recording
- Artifact storage
- Maid health check

### 2.2 Delete `_ipc_handle_simple` and `_ipc_handle_complex`

Dead code, superseded by `_core_handle_simple` and `_core_handle_complex`.

### 2.3 Tests

Test the unified `handle_message` path covers all three complexity levels,
records history, stores artifacts, sends reply with correct `reply_to`.

**Files:** `sdk/python/hivekernel_sdk/queen.py`, `sdk/python/tests/test_queen.py`

---

## Phase 3: Make handle_task a Thin Wrapper

Make `handle_task` delegate to the core-based methods, eliminating final duplication.

### 3.1 Rewrite `handle_task`

Keep `ctx.report_progress` calls for Execute stream callers (dashboard progress bar).
Replace `ctx.execute_on` / `ctx.spawn` / `ctx.kill` with `_core_handle_*` calls.
Delete old `_handle_simple`, `_handle_complex`, `_handle_architect`.

```python
async def handle_task(self, task, ctx):
    # Parse task, assess complexity (same as before)
    # ctx.report_progress still works for stream callers
    result = await self._core_handle_simple(description, trace_id)  # or _complex/_architect
    # Record history, store artifact (same code as handle_message path)
    return TaskResult(exit_code=result["exit_code"], output=result["output"], ...)
```

### 3.2 Tests

Verify existing Queen tests still pass. The mock targets change from
`ctx.execute_on` to `self.core.execute_task`.

**Files:** `sdk/python/hivekernel_sdk/queen.py`, `sdk/python/tests/test_queen.py`

---

## What We Do NOT Change

- **Orchestrator** -- keeps `handle_task` + SyscallContext. Worker delegation via
  `ctx.execute_on` inside asyncio.gather. Migration adds no value here yet.
- **Worker** -- keeps `handle_task`. Only uses `ctx.log` and `ctx.report_progress`.
- **Execute bidi stream** -- NOT removed. Still needed for Orchestrator -> Worker
  delegation and `CoreService.ExecuteTask` RPC.
- **SyscallContext** -- NOT removed. Still used by Orchestrator and Worker.

## Verification

1. `"C:\Program Files\Go\bin\go.exe" test ./internal/... -v` -- all Go tests pass
2. `python -m pytest sdk/python/tests/ -v` -- all Python tests pass
3. Start kernel with `--startup configs/startup-full.json`, send a `task_request`
   message to Queen via dashboard -- verify it processes through the unified path
4. Verify lead reuse works in the mailbox path (send two complex tasks in sequence)
