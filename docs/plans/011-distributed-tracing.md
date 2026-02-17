# Plan 011: Distributed Tracing (trace_id + span chain)

## Problem

When Assistant delegates a task to Queen, the full execution chain is invisible:
- Assistant(3) -> Queen(2) -> Lead(7) -> Worker(8), Worker(9), Worker(10)

Currently there's no way to correlate all events belonging to one delegation.
`reply_to` is a UUID for request-response correlation between two agents,
but it doesn't propagate down the spawn/execute chain.

Dashboard shows individual events but can't group them into a single "trace".

## Goal

Add distributed tracing so every event in a delegation chain carries a `trace_id`
and `span` showing the call path. Dashboard can then:
- Show all events for a trace_id (full picture of one delegation)
- Display the PID call chain (who spawned/called whom)
- Build a waterfall timeline view

## Design

### Trace format

```
trace_id = "t-<short_uuid>"     # generated once at top level
span     = "3/2/7/8"            # PID chain: caller/callee/...
```

Example flow:
```
assistant(3) sends to queen(2):     trace_id="t-abc123", span="3"
queen(2) spawns lead(7):           trace_id="t-abc123", span="3/2"
lead(7) spawns worker(8):          trace_id="t-abc123", span="3/2/7"
worker(8) executes:                trace_id="t-abc123", span="3/2/7/8"
```

### Propagation mechanism

trace_id flows through two channels:
1. **IPC messages**: in payload JSON (`"trace_id": "t-abc123"`)
2. **Task params**: in execute_task params map (`"trace_id": "t-abc123", "trace_span": "3/2"`)

Each agent that receives a trace_id appends its own PID to the span
before passing it down.

## Phases

### Phase 1: Proto + Go core (trace_id on ProcessEvent)

**`api/proto/core.proto`** -- add to ProcessEvent:
```protobuf
string trace_id = 17;
string trace_span = 18;
```

**`internal/process/eventlog.go`** -- add fields:
```go
TraceID   string `json:"trace_id,omitempty"`
TraceSpan string `json:"trace_span,omitempty"`
```

**`internal/kernel/syscall_handler.go`** -- in handleSend, extract trace_id
from message payload (JSON), populate on emitted ProcessEvent.

**`internal/kernel/grpc_core.go`**:
- In SendMessage emit, same as above
- In SubscribeEvents, map TraceID/TraceSpan to proto fields

Regenerate Go + Python protos.

### Phase 2: Assistant generates trace_id

**`sdk/python/hivekernel_sdk/assistant.py`** -- in `_handle_delegation`:
- Generate `trace_id = "t-" + uuid.uuid4().hex[:8]`
- Include in payload: `{"task": desc, "trace_id": trace_id}`
- Store trace_id in `_active_delegations[request_id]`

### Phase 3: Queen propagates trace_id

**`sdk/python/hivekernel_sdk/queen.py`** -- in `_process_message_task`:
- Extract trace_id from payload
- Pass to `_ipc_handle_simple` / `_ipc_handle_complex`

**`_ipc_handle_simple`**: pass trace_id + span to execute_task params
**`_ipc_handle_complex`**: pass trace_id + span to execute_task params

Include trace_id in the response payload too.

### Phase 4: Orchestrator propagates trace_id

**`sdk/python/hivekernel_sdk/orchestrator.py`** -- in handle_task:
- Read trace_id from task.params
- Pass to worker execute_on calls with extended span

### Phase 5: Dashboard trace view

**`sdk/python/dashboard/app.py`**:
- Add `trace_log: dict[str, list[dict]]` -- trace_id -> list of events
- In `apply_event`: if event has trace_id, append to trace_log
- Add `/api/traces` endpoint -- list recent traces
- Add `/api/traces/{trace_id}` endpoint -- all events for one trace
- In message_sent WS delta, include trace_id/trace_span

**`sdk/python/dashboard/static/`** -- optional: trace detail panel in UI

## File Summary

| # | File | Phase | Changes |
|---|------|-------|---------|
| 1 | `api/proto/core.proto` | P1 | trace_id + trace_span on ProcessEvent |
| 2 | `api/proto/hivepb/core.pb.go` | P1 | Regenerated |
| 3 | `sdk/python/hivekernel_sdk/core_pb2.py` | P1 | Regenerated + import fix |
| 4 | `internal/process/eventlog.go` | P1 | TraceID + TraceSpan fields |
| 5 | `internal/kernel/syscall_handler.go` | P1 | Extract trace from payload, emit |
| 6 | `internal/kernel/grpc_core.go` | P1 | Extract trace from payload, emit + stream map |
| 7 | `sdk/python/hivekernel_sdk/assistant.py` | P2 | Generate trace_id in delegations |
| 8 | `sdk/python/hivekernel_sdk/queen.py` | P3 | Propagate trace_id through IPC handlers |
| 9 | `sdk/python/hivekernel_sdk/orchestrator.py` | P4 | Propagate trace_id to workers |
| 10 | `sdk/python/dashboard/app.py` | P5 | trace_log, /api/traces, WS delta |

## Verification

1. `go build` + `go test ./internal/...`
2. Chat: "ask queen to research X"
3. Check `/api/traces` -- should show one trace with all events
4. Check `/api/traces/{id}` -- should show full chain: assistant -> queen -> lead -> workers
5. Check `/api/messages` -- trace_id visible on each message
6. Waterfall: events sorted by timestamp show the execution timeline
