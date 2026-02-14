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

- Bootstraps the system, spawns daemons (queen per VPS)
- Routes cross-VPS messages
- Makes migration decisions
- Human interface (receives user requests)
- Never dies; if it does, everything restarts

**Lifecycle:** permanent. No task pipeline — runs an event loop.

### 2.2 Daemon (Queen, Maid)

**One per VPS. Permanent, auto-restart on crash.**

#### Queen

**Tactical/Sonnet. Child of King.**

Purpose: local coordinator for a VPS. The "brain" that receives tasks
from King (or dashboard) and decides how to execute them.

Pipeline:
```
1. Receive task (from King, dashboard, or cron)
2. Decide strategy:
   - Simple task? Execute directly or spawn a task-agent
   - Complex task? Spawn a lead/orchestrator, delegate
   - Recurring? Store in crontab, trigger on schedule
3. Monitor execution (SIGCHLD on child completion)
4. Collect result, store artifact, report to King
5. Manage child lifecycle: reuse lead or kill, based on result
6. Wait for next task
```

Memory:
- Persistent across tasks (daemon never exits)
- Holds crontab (scheduled tasks)
- Knows which leads/agents are alive and available for reuse
- Accumulates knowledge about task patterns (which decomposition works)

#### Maid

**Tactical/Sonnet. Child of Queen.**

Purpose: per-VPS health and cleanup daemon.

Pipeline:
```
1. Periodically spawn monitoring tasks (memory, disk, zombie scan)
2. Collect results from monitors
3. Report anomalies to Queen
4. Clean up zombie processes that parents forgot to reap
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
| lead | parent (queen) | queen kills after project, or reuses |
| architect | supervisor | architect auto-sleeps, parent kills when done |
| agent | parent or user | session end -> kill |
| daemon | kernel auto-restart | if crash: restart. if kill: intentional |
| queen/maid orphans | maid detects | maid periodic zombie scan |

**Invariant:** every process has exactly one lifecycle manager (its parent).
The parent is responsible for killing children when they're no longer needed.
Supervisor is the safety net for zombies that parents forgot to collect.

---

## 7. Open Questions for Implementation

1. **Worker reuse protocol**: How does a lead signal "I want to reuse this worker"?
   Currently: just send another execute_on. Worker stays alive, handles next task.
   Question: should worker clear its LLM context between tasks? Configurable?

2. **Queen task routing**: How does Queen decide between spawning a task vs a lead?
   Heuristic: if task description is short/atomic -> task. If complex -> lead.
   Or: always ask LLM to classify.

3. **Budget inheritance for reused workers**: If a lead gets budget B for task 1,
   and budget B2 for task 2, do reused workers get fresh budget or carry over?

4. **Architect wake protocol**: When architect is woken for revision, does it
   reload its previous artifact automatically, or does parent pass it?

5. **Graceful shutdown timeout**: Currently 5s. Workers doing LLM calls
   can't exit gracefully in 5s. Increase to 15-30s? Or send SIGTERM
   then let LLM call finish naturally?

---

## 8. Roadmap

### 8.1 Architect Role & Strategic Planning Layer

**Status:** role defined, infrastructure exists (sleep/wake, artifacts), not yet used.

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

### 8.2 Live Queen

Make Queen an LLM daemon that receives tasks from dashboard/King,
assesses complexity, manages lead lifecycle, holds crontab.
Prerequisite for architect integration.

### 8.3 Worker Reuse & Memory

Define protocol for stateful vs stateless workers. Lead decides per-worker
whether to reuse (send new execute_on) or kill+respawn (fresh context).
Investigate agent memory via artifacts (load previous context on init).

### 8.4 Maid as Active Health Daemon

Maid spawns monitoring tasks periodically, detects resource anomalies,
cleans up forgotten zombies, reports to Queen. Currently just a struct
in `internal/daemons/` with no real implementation.

### 8.5 Cross-VPS Coordination

King routes tasks to queens on different VPS nodes. Migration of agents
between nodes. Queen-to-queen message routing. Requires cluster module
integration with real networking.
