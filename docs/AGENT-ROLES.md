# Agent Roles & Lifecycle Design

This document defines agent roles, their lifecycles, pipelines, and memory model
in HiveKernel. It serves as the design spec for implementation.

---

## 1. Role Overview

| Role | Lifetime | Manages children | Restart policy | Auto-exit on task complete |
|------|----------|-----------------|----------------|---------------------------|
| **kernel** | permanent | yes | always | no (never exits) |
| **daemon** | permanent | yes | always | no (waits for next task) |
| **agent** | session | yes | notify parent | no (waits for next task) |
| **architect** | until artifact | no | never | yes -> sleep |
| **lead** | parent decides | yes | notify parent | no (waits for next task) |
| **worker** | parent decides | limited | notify parent | configurable |
| **task** | one-shot | no | never | yes -> exit |

---

## 2. Role Definitions

### 2.1 Kernel (King)

**One per cluster. PID 1. Strategic/Opus.**

King is **init(1)** -- the root process that owns all kernel subsystems (28 total).
It does not know about specific agents.

- Bootstraps the system (process table, IPC, scheduler, event log, ...)
- Spawns agents from startup config (`--startup config.json`)
- Routes cross-VPS messages
- Provides syscall interface for all agents
- Never dies; if it does, everything restarts

**Lifecycle:** permanent. No task pipeline -- runs an event loop.

### 2.2 Daemon (Queen, Maid)

**One per VPS. Permanent, auto-restart on crash.**

#### Queen

**Tactical/Sonnet. Child of King. Optional -- kernel works without her.**

Purpose: task dispatcher. Receives tasks from dashboard or external callers,
assesses complexity, and routes to the appropriate strategy. Does NOT spawn
other daemons -- that is done via startup config.

Pipeline:
```
1. Receive task (from dashboard, external caller, or cron)
2. Assess complexity (task history -> heuristics -> LLM fallback)
3. Decide strategy:
   - Simple task? Spawn a task-agent (one-shot worker)
   - Complex task? Spawn/reuse a lead, delegate
   - Architecture needed? Spawn architect, then lead from plan
4. Monitor execution (SIGCHLD on child completion)
5. Collect result, store artifact
6. Manage lead lifecycle: reuse (idle pool with 5min timeout) or kill
7. Wait for next task
```

Memory:
- Persistent across tasks (daemon never exits)
- Task history with Jaccard similarity for complexity prediction
- Knows which leads are alive and available for reuse (idle pool)
- Accumulates knowledge about task patterns

#### Maid

**Operational. Child of King. Optional -- kernel handles zombies without her.**

Purpose: LLM-powered anomaly detection daemon. Note: zombie reaping and
crash detection are handled by the kernel itself (Supervisor + HealthMonitor).
Maid adds LLM-powered analysis on top.

Pipeline:
```
1. Receive health check task (from cron or manual trigger)
2. Query process tree and resource usage
3. Use LLM to detect anomalies beyond simple thresholds
4. Report anomalies to parent
5. Wait for next check cycle
```

Memory:
- Health metrics history
- Knows baseline "normal" for this VPS

### 2.3 Agent

**Session-lived. Notify parent on crash.**

Purpose: user-facing persistent agent. Has its own identity and can maintain
conversation context across multiple tasks. Example: "Leo" (personal assistant),
"Shop" (e-commerce agent).

Pipeline:
```
1. Initialized with persona (system prompt, tools, permissions)
2. Receives tasks from parent or user messages
3. Can spawn leads/workers for subtasks
4. Maintains conversation context across tasks
5. Lives until user session ends or parent kills
```

Memory:
- Conversation history (context window)
- User preferences (stored as artifacts)
- Session state persists between tasks

Lifecycle: parent decides when to kill. If crashed, parent is notified
(SIGCHLD) and can restart or replace.

### 2.4 Architect

**Ephemeral. Produces artifact, then sleeps.**

Purpose: one-time design/planning agent. Creates a document (architecture,
plan, spec) and goes dormant. Can be woken if the plan needs revision.

Pipeline:
```
1. Receive design task
2. Analyze requirements (may read existing artifacts)
3. Produce artifact (architecture doc, plan, schema)
4. Store artifact with VIS_SUBTREE or VIS_USER visibility
5. Auto-transition to sleeping state
6. (Optional) Wake on parent request for revision
```

Memory:
- Minimal. Output lives in artifacts, not in agent memory.
- On wake: reads own previous artifact to regain context.

Lifecycle: handle_task() -> return -> auto-sleep. Parent can wake (SIGCONT)
for revision or kill when no longer needed.

### 2.5 Lead (Orchestrator)

**Parent-managed. Coordinates workers.**

Purpose: project manager. Receives a complex task, decomposes it,
spawns/manages workers, collects results, synthesizes.

Pipeline:
```
1. Receive task from parent (queen or agent)
2. Analyze and decompose into subtasks (via LLM)
3. Spawn workers (or reuse existing idle workers)
4. Delegate subtasks via execute_on
5. Monitor progress (workers report via syscalls)
6. Handle failures (retry, reassign, escalate)
7. Synthesize results into final output
8. Return result to parent
9. Stay alive — parent decides: next task or kill
```

Memory:
- Task decomposition patterns (learns what works)
- Worker pool state (which workers are alive, their capabilities)
- Can accumulate domain knowledge across related tasks

Lifecycle: stays alive after task completion. Parent (Queen) decides:
- Same domain next task? -> reuse (execute_on with new task)
- Different domain? -> kill, spawn fresh lead
- No more tasks? -> kill

Worker management: Lead is responsible for its workers' lifecycle.
On task complete, Lead should either:
- Kill idle workers (clean up resources)
- Keep workers alive for reuse (if next task is expected soon)
On Lead kill: kernel reparents orphaned children to Lead's parent.

### 2.6 Worker

**Parent-managed. Executes individual subtasks.**

Purpose: the actual executor. Receives a specific, well-defined task
and completes it.

Pipeline:
```
1. Receive task from parent (lead or agent)
2. Execute via LLM call
3. Report progress (report_progress syscall)
4. Return result
5. Behavior after task depends on parent's intent:
   a) Parent sends new task -> execute again
   b) Parent kills -> exit
   c) No activity -> stay idle (parent manages lifecycle)
```

Memory:
- Minimal by default (stateless worker)
- Can accumulate context if parent sends related tasks sequentially
- Context resets on kill+respawn (stateless restart)

Lifecycle:
- Worker does NOT auto-exit after task. It stays alive.
- Parent (Lead) decides: reuse or kill.
- This allows parent to choose between:
  - Stateless: kill after each task (fresh context, no leaks)
  - Stateful: reuse for related tasks (accumulated context)
- If worker crashes, parent is notified (SIGCHLD), can respawn.

### 2.7 Task

**One-shot. Auto-exits after completion.**

Purpose: atomic, fire-and-forget operation. Run a test, parse a file,
check a URL, format output. No children, no coordination.

Pipeline:
```
1. Receive task
2. Execute (typically a single LLM call or tool invocation)
3. Return result
4. Auto-exit -> zombie -> parent collects -> reap
```

Memory: none. Stateless by definition. Cannot spawn children.

Lifecycle: handle_task() -> return -> process exits automatically.
Parent collects result via SIGCHLD + wait_child. Supervisor reaps
zombie after timeout if parent doesn't collect.

---

## 3. Auto-Exit Rules

The runner (Python) determines post-task behavior based on role:

| Role | After handle_task() returns |
|------|---------------------------|
| kernel | N/A (event loop, no handle_task) |
| daemon | Stay alive, wait for next Execute stream |
| agent | Stay alive, wait for next Execute stream |
| architect | Transition to sleeping state |
| lead | Stay alive, wait for next Execute stream |
| worker | Stay alive, wait for next Execute stream |
| task | Exit process -> zombie -> reap |

Only **task** auto-exits. All other roles stay alive for potential reuse.
Parent explicitly kills when done.

This is analogous to Linux:
- `task` = `exec command` (runs, exits)
- `worker` = a process waiting in `read()` loop (stays alive between inputs)
- `daemon` = a service (`systemd` restarts if crashed)

---

## 4. Memory Model

### 4.1 Volatile (lost on kill)

- **Context window**: LLM conversation history within current process
- **Python object state**: any in-memory variables
- Current task progress

### 4.2 Persistent (survives kill)

- **Artifacts**: stored via `store_artifact` syscall, live in kernel registry
  - VIS_PRIVATE: only this PID (lost when process removed)
  - VIS_USER: all agents with same USER
  - VIS_SUBTREE: agent + all descendants
  - VIS_GLOBAL: everyone
- **Task results**: stored in LifecycleManager until parent collects
- **Event log**: JSONL on disk, captures full history

### 4.3 Future: Agent Memory

Not yet implemented, but design space:

- **Short-term**: context window (current, volatile)
- **Long-term**: stored as artifacts with VIS_PRIVATE, loaded on init
- **Shared**: artifacts with broader visibility
- **Episodic**: event log provides audit trail

When a worker is killed and respawned, it starts fresh (stateless).
When a worker is reused (new Execute stream), it retains Python state
and can access previous artifacts.

---

## 5. Standard Pipelines

### 5.1 User Task Pipeline (target architecture)

```
User -> Dashboard -> Queen
                       |
                       v
              Queen decides strategy
              /         |          \
       Simple      Medium       Complex (future, see 8.1)
         |            |              |
         v            v              v
      Spawn task   Spawn lead    Spawn architect -> plan artifact
      (one-shot)   (orchestrator)   -> spawn lead from plan
         |            |              |
         v            v              v
      Execute,    Decompose,     Lead reads plan,
      return      spawn workers, spawns workers,
      result      delegate,      executes per spec
         |        synthesize         |
         v            |              v
      Task exits      v          Return result
      (auto-zombie)  Return      (if fails: wake architect,
                     result       revise, retry)
                         \       /
                          v    v
                    Queen collects result
                    Decides: reuse lead? kill?
                    Stores artifact
                    Reports to King/Dashboard
```

Note: the "Complex" branch (architect) is a planned extension (section 8.1).
Initial implementation covers Simple and Medium paths only.

### 5.2 Current Pipeline (battle test)

```
User -> Dashboard -> spawn orchestrator directly
                     execute_on(orchestrator, task)
                     orchestrator works...
                     result returned to dashboard
                     (orchestrator stays alive forever)
```

Gap: Queen is not involved. No lifecycle management after task.

### 5.3 Migration Path

To get from 5.2 to 5.1:
1. Implement task auto-exit (runner checks role, exits for task)
2. Make Queen an LLM agent (receives tasks, decides strategy)
3. Dashboard talks to Queen instead of spawning directly
4. Queen manages lead lifecycle (reuse/kill decisions)
5. Lead manages worker lifecycle (cleanup after task)

---

## 6. Kill & Cleanup Responsibilities

| Who dies | Who manages cleanup | Mechanism |
|----------|-------------------|-----------|
| task | supervisor auto-reap | task auto-exits -> zombie -> reap after 60s |
| worker | parent (lead) | lead kills after task, or reuses |
| lead | parent (queen) | queen kills after project, or reuses (idle pool) |
| architect | supervisor | architect auto-sleeps, parent kills when done |
| agent | parent or user | session end -> kill |
| daemon | kernel auto-restart | if crash: restart. if kill: intentional |
| orphans | supervisor | supervisor reaps zombies after 60s, reparents children |

**Invariant:** every process has exactly one lifecycle manager (its parent).
The parent is responsible for killing children when they're no longer needed.
Supervisor is the safety net for zombies that parents forgot to collect.

---

## 7. Open Questions (Resolved)

1. **Worker reuse protocol**: Resolved. Lead sends another `execute_on`. Worker
   stays alive, handles next task. `_task_memory` accumulates context. Lead decides
   whether to reuse (stateful) or kill+respawn (stateless).

2. **Queen task routing**: Resolved. Three-step assessment: task history (Jaccard
   similarity) -> heuristic keywords -> LLM fallback. Routes to simple/complex/architect.

3. **Budget inheritance for reused workers**: Not yet needed in practice. Workers
   use parent's budget implicitly (token consumption recorded against the worker PID).

4. **Architect wake protocol**: Architect reads its own previous artifact on wake.
   Parent passes the original task description, architect loads context from stored plan.

5. **Graceful shutdown timeout**: Resolved. 5s agent shutdown + 2s kill timeout.
   gRPC keepalive (ping 5s, timeout 2s). Overall 15s shutdown deadline with
   double Ctrl+C pattern (first = graceful, second = force kill).

---

## 8. Roadmap

### 8.1 Architect Role & Strategic Planning Layer

**Status:** Role defined, infrastructure exists (sleep/wake, artifacts). ArchitectAgent
implemented as LLM-powered planner (produces structured JSON plan with groups,
risks, acceptance criteria). Used by Queen for complex/novel tasks.

**Why it will be needed:**

Queen will need to assess task complexity to decide between spawning a task
vs a lead. That assessment itself requires strategic thinking — a tactical
model may misjudge what's simple and what's not. When sonnet-level
decomposition produces poor subtask splits (too coarse, missing dependencies,
wrong division of concerns), the answer is a dedicated planning step with
a strategic-tier model.

Additionally, complex tasks benefit from an iterative cycle: plan, execute,
discover the plan was wrong, revise plan, re-execute. The architect's
sleep/wake model is designed exactly for this — architect sleeps after
producing a plan, lead executes, if execution reveals problems the architect
is woken for revision without losing the planning context stored in artifacts.

**Pipeline with architect (future 5.1 extension):**

```
User -> Dashboard -> Queen
                       |
                       v
              Queen assesses complexity
              /         |          \
       Simple      Medium       Complex/novel
         |            |              |
         v            v              v
      task          lead         architect (Opus)
     (one-shot)   (decomposes      |
         |        directly)     Produces plan artifact
         v            |         (architecture, dependencies,
      result          v          subtask specs, acceptance criteria)
                   workers           |
                      |              v
                      v          Goes to sleep
                   result            |
                                     v
                                 Queen reads artifact,
                                 spawns lead
                                     |
                                     v
                                 Lead reads plan artifact,
                                 spawns workers per spec
                                     |
                                     v
                                 If execution fails:
                                   wake architect (SIGCONT)
                                   architect revises plan
                                   lead retries
                                     |
                                     v
                                 Result to Queen
```

**When to implement:** after Queen is a live LLM agent and has encountered
tasks where tactical-tier decomposition is insufficient.

### 8.2 Live Queen -- COMPLETE

Queen is a live LLM daemon. Receives tasks from dashboard/external callers,
assesses complexity (history -> heuristics -> LLM), manages lead lifecycle
(idle pool with 5min timeout, reaper), routes to 3 strategies (simple/complex/architect).

### 8.3 Worker Reuse & Memory -- COMPLETE

Workers stay alive between tasks. Lead sends `execute_on` for reuse.
`WorkerAgent._task_memory` accumulates context across sequential tasks.
Kill+respawn gives fresh context (stateless restart).

### 8.4 Maid -- PARTIAL

Go Maid (`internal/daemons/`) deleted -- Supervisor + HealthMonitor handle
zombie reaping and crash detection. Python MaidAgent exists as optional
LLM-powered anomaly detection daemon.

### 8.5 Cross-VPS Coordination

King routes tasks to queens on different VPS nodes. Migration of agents
between nodes. Requires cluster module integration with real networking.
Mechanism is in kernel (NodeRegistry, Connector, MigrationManager),
migration decisions should move to agents.
