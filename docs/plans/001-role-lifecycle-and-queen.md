# Plan 001: Role-Based Lifecycle & Live Queen

**Based on:** `docs/AGENT-ROLES.md`
**Status:** draft
**Scope:** implement auto-exit for tasks, live Queen agent, dashboard -> Queen routing

---

## Overview

Three problems to solve:
1. Task-role processes don't auto-exit after completing (stay alive forever)
2. Queen is a virtual demo process, not a real LLM agent
3. Dashboard bypasses Queen, spawns orchestrators directly

## Phases

### Phase 1: Task Auto-Exit

**Goal:** processes with role=task automatically exit after handle_task() returns.

**Changes:**

`sdk/python/hivekernel_sdk/agent.py` — in `_make_servicer`, after Execute's
`runner_task` completes (line 230), check `agent._role`. If role is "task",
initiate graceful server shutdown:

```python
# After runner_task completes in Execute():
await runner_task
await writer_task
reader_task.cancel()

# Auto-exit for one-shot roles.
if agent._role == "task":
    asyncio.get_event_loop().call_later(0.5, server_ref.stop, 2)
```

The 0.5s delay lets the final PROGRESS_COMPLETED message flush on the stream.
`server.stop(2)` gives 2s grace for gRPC cleanup. After that, the Python
process exits, Manager catches `cmd.Wait()`, supervisor handles lifecycle.

Need: the Servicer needs a reference to the gRPC server. Currently the server
is created in `runner.py`, not accessible from Servicer. Two options:
- a) Pass server reference into agent (e.g. `agent._server = server`)
- b) Agent sets a flag, runner checks it after `wait_for_termination`

Option (a) is simpler. Runner already sets `agent._core`, adding `_server` is
the same pattern.

**Test:** spawn a task-role agent, execute_on it, verify process exits
automatically and goes zombie -> reaped.

**Files:**
- `sdk/python/hivekernel_sdk/agent.py` — auto-exit logic in Execute
- `sdk/python/hivekernel_sdk/runner.py` — pass server ref to agent

**Risk:** low. Only affects role=task, other roles unchanged.

---

### Phase 2: Queen Agent

**Goal:** Queen is a real Python LLM agent (daemon, tactical/sonnet) that
receives tasks and decides how to execute them.

**New file:** `sdk/python/hivekernel_sdk/queen.py`

```
class QueenAgent(LLMAgent):
    async def handle_task(self, task, ctx):
        # 1. Assess task complexity via LLM
        # 2. Simple? -> spawn task, execute_on, return result
        # 3. Complex? -> spawn lead (orchestrator), execute_on, collect result
        # 4. Kill children after collecting
        # 5. Store result as artifact
        # 6. Return result to caller
```

Queen's decision logic (first iteration, simple heuristic):
- Ask LLM: "Is this task simple (one step) or complex (needs decomposition)?
  Reply with JSON: {complexity: 'simple'|'complex'}"
- Simple -> spawn task-role worker, single LLM call
- Complex -> spawn lead (OrchestratorAgent), full decomposition pipeline

Queen stays alive after task (daemon role). Waits for next Execute call.

**Lifecycle management by Queen:**
- After execute_on(lead) returns, Queen kills the lead
- After execute_on(task) returns, task auto-exits (Phase 1)
- Queen tracks which leads are alive in case of reuse (future)

**Files:**
- `sdk/python/hivekernel_sdk/queen.py` — NEW, QueenAgent class
- `sdk/python/hivekernel_sdk/__init__.py` — export QueenAgent

**Needs:** a simple task agent (TaskAgent) for the "simple" branch.
Could reuse WorkerAgent or create a minimal one.

**Risk:** medium. LLM classification may be inconsistent. Start with
always-complex (always spawn lead) and add simple branch later.

---

### Phase 3: Kernel Spawns Real Queen on Startup

**Goal:** instead of virtual demo queen, kernel spawns a real Python
QueenAgent process on startup.

**Changes:**

`cmd/hivekernel/main.go` — in `demo()` function (or replace it):
- Spawn queen with RuntimeImage = "hivekernel_sdk.queen:QueenAgent"
- Remove virtual demo-worker spawn
- Queen starts as real Python process, gets Init, waits for tasks

```go
queen, err := king.SpawnChild(process.SpawnRequest{
    ParentPID:     king.PID(),
    Name:          "queen@vps1",
    Role:          process.RoleDaemon,
    CognitiveTier: process.CogTactical,
    Model:         "sonnet",
    User:          "root",
    Limits:        cfg.DefaultLimits,
    RuntimeType:   "RUNTIME_PYTHON",
    RuntimeImage:  "hivekernel_sdk.queen:QueenAgent",
})
```

**Files:**
- `cmd/hivekernel/main.go` — real queen spawn

**Risk:** low. Replaces demo with real agent.

---

### Phase 4: Dashboard Talks to Queen

**Goal:** `/api/run-task` delegates to Queen instead of spawning orchestrators
directly.

**Changes:**

`sdk/python/dashboard/app.py` — rewrite `api_run_task`:

```python
@app.post("/api/run-task")
async def api_run_task(body: dict):
    task_text = body.get("task", "")
    # Find queen PID (first daemon child of king)
    queen_pid = find_queen_pid()
    # Execute task on queen — she decides the strategy
    result = await stub.ExecuteTask(
        target_pid=queen_pid,
        description=task_text,
        params={...},
    )
    return result
```

Dashboard no longer knows about orchestrators or workers. It just sends a task
to Queen and gets a result back. Queen handles everything internally.

**Finding Queen PID:** dashboard can find queen by name ("queen@vps1") via
tree_cache, or kernel could expose a dedicated RPC. Simplest: on startup,
dashboard finds the daemon child of PID 1.

**Files:**
- `sdk/python/dashboard/app.py` — rewrite run-task endpoint

**Risk:** low. Dashboard becomes simpler (less logic, just relays to Queen).

---

### Phase 5: Lead Lifecycle Cleanup

**Goal:** Lead (orchestrator) properly cleans up workers and reports back.
Queen properly cleans up leads.

**Current state:** orchestrator already kills workers in handle_task() line 162.
This is correct — lead manages worker lifecycle. But the kill loop is slow
(5s per worker due to graceful shutdown timeout).

**Changes:**

1. **Parallel worker kill in orchestrator.py:**
   Currently sequential `for wpid: await ctx.kill(wpid)`. Change to parallel:
   spawn all kills concurrently, await all.

2. **Graceful shutdown timeout:**
   `internal/runtime/manager.go` — increase graceful timeout from 5s to 15s.
   Workers doing LLM calls need more time to finish.
   Or: send Shutdown RPC first, then wait, then force kill.

3. **Task auto-exit for workers (Phase 1 continuation):**
   Workers are spawned as role=task by orchestrator (line 79). With Phase 1,
   they will auto-exit after execute_on completes. Orchestrator's explicit
   kill becomes unnecessary — workers exit on their own.

   After Phase 1, orchestrator cleanup simplifies to: just wait for workers
   to die (they auto-exit), or kill any stuck ones.

**Files:**
- `sdk/python/hivekernel_sdk/orchestrator.py` — parallel kill or remove kill loop
- `internal/runtime/manager.go` — adjust graceful timeout (optional)

---

## Implementation Order

```
Phase 1 (task auto-exit)
  |
  v
Phase 2 (QueenAgent)  +  Phase 5 (cleanup improvements)
  |                            |
  v                            v
Phase 3 (real queen spawn)
  |
  v
Phase 4 (dashboard -> queen)
  |
  v
Test end-to-end: user -> dashboard -> queen -> lead -> workers -> result
```

Phases 2 and 5 can run in parallel. Phase 3 depends on Phase 2.
Phase 4 depends on Phase 3.

## Verification

After all phases:
1. Start kernel -> queen spawns as real Python agent (visible in dashboard)
2. Open dashboard, run task -> goes to Queen, not direct orchestrator spawn
3. Queen decides strategy, spawns lead, lead spawns workers
4. Workers complete, auto-exit (role=task), go zombie, get reaped
5. Lead returns result to Queen, Queen returns to dashboard
6. Queen kills lead (or keeps for next task)
7. Queen stays alive, ready for next task
8. Dashboard shows full tree lifecycle in real time
9. Logs show clean lifecycle: no HealthMonitor kills, no stuck processes
