# Plan 002: System Health & Queen Evolution

**Based on:** `docs/AGENT-ROLES.md`, roadmap sections 8.2-8.5
**Status:** in progress
**Depends on:** Plan 001 (done)

---

## Overview

Five phases building on the live Queen foundation. Focus: make the system
robust (instant death detection, health daemon) then evolve Queen's intelligence
(crontab, lead reuse, worker reuse) and add architect role for complex tasks.

## Execution Order

```
Phase 1: Process Exit Watcher (Go, ~50 LOC)
    |
Phase 2: Maid Daemon (Python agent, health/cleanup)
    |
Phase 3: Queen Improvements (reuse leads, smarter routing)
    |
Phase 4: Worker Reuse Protocol (stateful vs stateless)
    |
Phase 5: Architect Role (strategic planning, sleep/wake)
```

---

### Phase 1: Process Exit Watcher

**Goal:** When a Python agent process dies (auto-exit, crash, or kill), the Go
runtime Manager instantly detects it and transitions the process to zombie.

**Problem:** Currently only HealthMonitor detects dead processes (~20s delay via
3 failed pings at 10s interval). This makes death detection slow and unreliable
for features that need fast SIGCHLD delivery.

**Solution:** Add a `cmd.Wait()` goroutine in Manager.spawnReal() that watches
for process exit. When detected:

1. `Registry.SetState(pid, StateZombie)`
2. `Signals.NotifyParent(pid, exitCode, output)` — SIGCHLD
3. `Manager.cleanupRuntime(pid)` — close gRPC conn, remove from map
4. `HealthMonitor.Remove(pid)` — stop pinging dead process

```go
// In Manager.spawnReal(), after cmd.Start():
go func() {
    err := cmd.Wait()
    exitCode := 0
    if exitErr, ok := err.(*exec.ExitError); ok {
        exitCode = exitErr.ExitCode()
    }
    log.Printf("[runtime] PID %d process exited (code %d)", pid, exitCode)
    // Notify lifecycle system.
    if m.onProcessExit != nil {
        m.onProcessExit(pid, exitCode)
    }
}()
```

Manager gets an `OnProcessExit(func(pid, exitCode))` callback, wired in main.go
to do SetState + NotifyParent + cleanup. Same pattern as HealthMonitor.OnUnhealthy.

**HealthMonitor interaction:** After exit watcher handles death, HealthMonitor
should skip dead/zombie PIDs. Already partly handled (HealthMonitor pings
runtimes map, exit watcher removes from map). But add explicit zombie check
in HealthMonitor.check() for safety.

**Files:**
- `internal/runtime/manager.go` — exit watcher goroutine, OnProcessExit callback
- `cmd/hivekernel/main.go` — wire OnProcessExit
- `internal/runtime/health.go` — skip zombie/dead in check()
- `internal/runtime/manager_test.go` — test exit detection (optional)

**Risk:** low. Additive change, doesn't break existing flow.

**Status:** DONE (commit d0c9bc2)

---

### Tech Debt (discovered during Phase 1)

**Shutdown RPC doesn't stop the Python process.**

`Shutdown` handler in `agent.py` calls `on_shutdown()` and returns a response,
but never calls `server.stop()`. The Python process stays blocked on
`server.wait_for_termination()` forever. Result: StopRuntime always hits the
5s graceful timeout and force-kills non-task agents.

**Fix:** In `agent.py` Shutdown handler, trigger server stop after on_shutdown:
```python
async def Shutdown(self, request, context):
    snapshot = await agent.on_shutdown(reason)
    if agent._server is not None:
        asyncio.create_task(agent._server.stop(grace=2))
    return ShutdownResponse(state_snapshot=snapshot or b"")
```

**Scope:** one line in `agent.py`. Should be done before Phase 2 (Maid will
also be killed via StopRuntime and will hit the same timeout).

---

### Phase 2: Maid Daemon

**Goal:** Per-VPS health and cleanup daemon. Spawned by Queen on startup.

**Responsibilities:**
- Periodic zombie scan: find zombie processes whose parents didn't reap
- Resource monitoring: track total token usage, process count
- Stale process detection: agents idle too long without tasks
- Report anomalies to Queen via escalate syscall

**Implementation:**

New file: `sdk/python/hivekernel_sdk/maid.py`

```python
class MaidAgent(LLMAgent):
    async def handle_task(self, task, ctx):
        # Maid runs periodic health checks as its "task"
        # Each check cycle:
        # 1. List all children of Queen (siblings)
        # 2. Check for zombies older than threshold
        # 3. Report anomalies
        # 4. Return summary
```

Maid is spawned by Queen in `on_init()` (not via task). Queen creates Maid
as a daemon child on first startup.

Maid's periodic checks are triggered by Queen sending execute_on periodically,
or Maid could self-schedule using a timer in handle_task that loops.

**Simpler first version:** Maid is a task-role agent that Queen spawns
periodically (every 60s), does one health scan, returns report, auto-exits.
No persistent daemon needed for v1.

**Files:**
- `sdk/python/hivekernel_sdk/maid.py` — NEW
- `sdk/python/hivekernel_sdk/queen.py` — spawn maid periodically (or on_init)
- `sdk/python/hivekernel_sdk/__init__.py` — export MaidAgent

**Risk:** low. Independent agent, Queen integration is optional.

---

### Phase 3: Queen Improvements

**Goal:** Queen becomes smarter about lifecycle management.

**Changes:**

1. **Lead reuse:** After complex task completes, Queen can keep the lead alive
   and reuse it for the next similar task instead of kill+respawn.
   - Track active leads: `{lead_pid: {"domain": ..., "last_used": ...}}`
   - On new complex task: check if an idle lead exists, reuse via execute_on
   - Kill leads after idle timeout (e.g. 5 minutes)

2. **Better complexity assessment:** Instead of one LLM call, use structured
   heuristics first (keyword matching, task length) and only call LLM for
   ambiguous cases. Saves tokens on obvious cases.

3. **Task history:** Queen remembers results of recent tasks (in-memory list).
   Can inform future complexity assessment ("last time a similar task was
   complex and needed 5 workers").

4. **Maid integration:** Queen periodically asks Maid for health report,
   acts on anomalies (kill stuck processes, restart crashed leads).

**Files:**
- `sdk/python/hivekernel_sdk/queen.py` — lead reuse, history, maid integration

**Risk:** medium. Lead reuse needs careful state tracking.

---

### Phase 4: Worker Reuse Protocol

**Goal:** Leads can reuse workers across subtasks instead of kill+respawn.

**Changes:**

1. **Worker pool in orchestrator:** Instead of spawning N workers for N subtasks,
   spawn a pool and assign tasks round-robin or on-demand.

2. **Stateful vs stateless decision:** Lead decides per-worker:
   - Related subtasks -> reuse same worker (accumulated context)
   - Unrelated subtasks -> kill and respawn (fresh context)

3. **Worker idle management:** Workers that finish a subtask wait for next
   execute_on instead of auto-exiting. Lead kills them when done.
   - This means workers should be role=worker (not task) when reuse is intended
   - Orchestrator chooses role based on reuse strategy

**Files:**
- `sdk/python/hivekernel_sdk/orchestrator.py` — worker pool, reuse logic
- `sdk/python/hivekernel_sdk/worker.py` — handle multiple sequential tasks

**Risk:** medium. Changes worker lifecycle model.

---

### Phase 5: Architect Role

**Goal:** Strategic-tier planning for complex/novel tasks.

**When activated:** Queen encounters a task where:
- Tactical-tier decomposition fails (too coarse, missing dependencies)
- Task is novel (no history of similar tasks)
- Task explicitly requests architecture/planning

**Pipeline:**
1. Queen spawns Architect (strategic/opus tier)
2. Architect analyzes task, produces plan artifact (JSON: subtasks, dependencies,
   acceptance criteria, worker specs)
3. Architect goes to sleep (auto-sleep after storing artifact)
4. Queen reads plan artifact, spawns Lead
5. Lead reads plan artifact, executes per spec
6. If execution fails: Queen wakes Architect for plan revision
7. On success: Queen stores final result, kills Architect

**Implementation:**

New file: `sdk/python/hivekernel_sdk/architect.py`

```python
class ArchitectAgent(LLMAgent):
    async def handle_task(self, task, ctx):
        # 1. Analyze requirements
        # 2. Produce detailed plan (JSON artifact)
        # 3. Store as artifact with VIS_SUBTREE
        # 4. Return plan summary
        # (Process auto-sleeps after return — architect role)
```

**Sleep/wake mechanism:** Currently not implemented in runner. Need:
- Architect role in runner: after handle_task returns, enter sleep state
  (don't exit, don't accept new tasks, just wait for SIGCONT)
- SIGCONT handler: wake agent, load previous artifact, accept new task
- Or simpler: Architect is role=task, auto-exits. On revision, Queen
  spawns a new Architect with the old artifact as context.

**Files:**
- `sdk/python/hivekernel_sdk/architect.py` — NEW
- `sdk/python/hivekernel_sdk/queen.py` — architect spawning, plan reading
- `sdk/python/hivekernel_sdk/agent.py` — sleep state (optional, v2)

**Risk:** high. New role, new lifecycle pattern, opus-tier LLM costs.
Start with simplified version (architect = task, no sleep/wake).

---

## Verification

After each phase:

**Phase 1:** Start kernel, spawn task-role agent, verify instant zombie
transition in logs (no 20s HealthMonitor delay). Dashboard shows immediate
state change.

**Phase 2:** Start kernel, send task to Queen, verify Maid spawns and
reports health. Dashboard shows Maid in tree.

**Phase 3:** Send two complex tasks in sequence, verify second task reuses
the lead from first. Logs show "reusing lead PID X" instead of new spawn.

**Phase 4:** Send complex task with 5 subtasks, verify worker pool reuse
(fewer spawns than subtasks if pool is used). Logs show worker assignment.

**Phase 5:** Send novel/complex task, verify Architect spawns, plan artifact
created, Lead reads plan. Dashboard shows full tree: Queen -> Architect +
Lead -> Workers.
