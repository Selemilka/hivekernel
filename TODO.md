# HiveKernel TODO

Unified task tracker. Priorities: P0 (blocks usage) > P1 (enables workflows) > P2 (architecture) > P3 (enhancements) > P4 (future).

Status: `[ ]` todo, `[~]` in progress, `[x]` done.

---

## P0 -- Critical (blocks practical use)

### [ ] 1. Context persistence across restarts
Agents lose all state (memory, conversation history, task progress) when kernel restarts. Need:
- **Agent memory snapshots** -- serialize AgentMemory (session history, long-term knowledge, summaries) to disk or artifact store on shutdown, restore on startup
- **Process tree checkpoint** -- save running process tree + states so kernel can resume (not just cold-start from config)
- **In-flight task recovery** -- tasks interrupted by restart should be re-queued or reported as failed
- Checkpoint format: JSONL or SQLite in `data/` directory
- Trigger: periodic (every N minutes) + on graceful shutdown (SIGTERM)

### [x] 2. Queen mailbox unification (plan 012)
Queen has duplicate code paths: `handle_task` (via execute_task) and `handle_message` (via mailbox) with the same business logic in both. This is the #1 architectural debt.

Fix: Queen **receives** all tasks via `handle_message()` (single entry point). Queen **delegates** to children via `core.execute_task()` (synchronous, waits for result -- this is fine, not legacy). `handle_task()` becomes a thin adapter that calls the same internal methods.

Note: `execute_task` is NOT legacy -- it's the right tool for parent->child delegation (especially atomic workers in parallel). What's legacy is receiving tasks through both entries with duplicated logic.

**Plan:** `docs/plans/012-mailbox-migration.md`
- Phase 0: Add missing CoreClient RPCs (list_siblings, wait_child)
- Phase 1: Extract core-compatible Queen methods
- Phase 2: Unify handle_message as single entry
- Phase 3: handle_task becomes thin wrapper

### [x] 3. Wire supervisor auto-restart
`Supervisor.onRestart` callback exists but is never set in `main.go`. Daemon crash recovery is architecturally correct but effectively a no-op. One-line fix to connect it to the runtime manager's `StartRuntime()`.

**Files:** `cmd/hivekernel/main.go`, `internal/process/supervisor.go`, `internal/runtime/manager.go`

---

## P1 -- High (enables real workflows)

### [ ] 4. Request/LLM trace logging
Current distributed tracing (plan 011) tracks message flow between agents (trace_id/span). Need a separate or extended system for **request-level traces**:
- **LLM call logging** -- each LLM API call: timestamp, agent PID, model, prompt tokens, completion tokens, latency, cost estimate
- **Tool call logging** -- every tool invocation: tool name, arguments, result summary, duration
- **Request lifecycle** -- from user input to final response, with all intermediate steps
- Consider: extend current trace_id system with span types (`span_type: "ipc" | "llm_call" | "tool_call"`) vs separate log stream
- Storage: append-only JSONL per session, queryable from dashboard
- Dashboard: request timeline view (waterfall), token usage breakdown

**Related plan:** `docs/plans/011-distributed-tracing.md` (NOT implemented, covers IPC tracing only)

### [ ] 5. Comprehensive logging improvements
Currently missing visibility into:
- **Inbox message history** -- messages delivered to agents not visible after consumption; need persistent message log with sender/receiver/timestamp/payload
- **Cron execution log** -- cron jobs fire but results/failures not always visible; need: last_run, next_run, exit status, output for each cron entry
- **Agent-level structured logs** -- agents call `ctx.log()` but logs are fire-and-forget; need queryable log storage per PID with levels (debug/info/warn/error)
- **Dashboard log panels** -- filterable log viewer: by PID, by level, by time range, by trace_id

**Related plan:** `docs/plans/007-debug-logging-and-dashboard.md` (NOT implemented, covers slog migration + debug logging)

### [ ] 6. Merge queue for multi-agent code generation
Multiple agents writing code simultaneously need conflict resolution:
- Git commit gating (only one agent commits at a time)
- Conflict detection before merge
- Build/test validation before accepting merge
- Automatic rebase or retry on conflict
- Queue with priority ordering (matches task priority)

This is the biggest gap compared to systems like Longshot (1200 commits/hour with 200+ agents).

### [ ] 7. Practical swarm workflow (end-to-end web development)
Design and implement a workflow where the swarm solves real tasks, e.g. "build a landing page with AI chat integration". Requirements:
- **Daemon hierarchy with responsibility zones** -- e.g. ProjectManager (strategic), FrontendLead (tactical), BackendLead (tactical), QA (tactical)
- **Dynamic worker spawning** -- leads spawn workers for specific subtasks (write HTML, implement API, write tests)
- **Tool availability** -- workspace_tools (file I/O, shell), web_tools (fetch, search), git tools
- **Artifact pipeline** -- workers produce files, leads review/integrate, PM validates against spec
- **Frozen spec** -- immutable task specification agents can reference but not modify
- **Example config** -- `configs/startup-webdev.json` with full agent tree, prompts, tools
- **Example prompts** -- `prompts/project-manager.md`, `prompts/frontend-lead.md`, etc.
- **Demo script** -- give task, watch swarm work, get deployable output

**Depends on:** #2 (mailbox unification), #6 (merge queue), #1 (context persistence for long tasks)

---

## P2 -- Medium (architecture improvements)

### [ ] 8. Acknowledge kernel Scheduler status
`internal/scheduler/scheduler.go` (220 lines, 56 tests) is a pull-based task queue built for flat worker pools. In the hierarchical tree model, parents always route work to children -- making a centralized scheduler redundant. Options:
- **(a)** Keep as library for agent-internal use (Queen's lead pool could use it)
- **(b)** Remove entirely (dead code, no callers)
- **(c)** Repurpose if/when generic worker pools are needed (TODO #7)

Decision deferred until swarm workflow design. See `docs/MAILBOX.md` section 8 for rationale.

### [ ] 9. Distributed tracing implementation
Add trace_id + span propagation through IPC messages and execute_on chains. Dashboard trace view with waterfall timeline.

**Plan:** `docs/plans/011-distributed-tracing.md` (NOT implemented)
- Phase 1: Proto + Go core (trace_id on ProcessEvent)
- Phase 2: Assistant generates trace_id
- Phase 3: Queen propagates trace_id
- Phase 4: Orchestrator propagates trace_id
- Phase 5: Dashboard trace view

### [ ] 10. DAG execution engine
Orchestrator decomposes tasks into parallel groups, but there's no dependency graph. A DAG engine would allow: A depends on B, fan-out/fan-in, conditional branches, retry individual nodes.

### [ ] 11. Frozen spec pattern (objective drift prevention)
Before a multi-agent task starts, freeze the specification as an immutable artifact. Agents get read-only view + separate mutable decision log. Prevents goal drift during iteration.

### [ ] 12. Application-level reconciler
Current health monitoring only checks "is the process alive" (heartbeat). A reconciler daemon should verify semantic health: run tests, check for regressions, validate outputs against acceptance criteria. Spawn fix-tasks when issues found.

---

## P3 -- Enhancements

### [ ] 13. Consolidate log directory + debug logging
- **Log directory** -- event logs write to both `logs/` (root) and `internal/kernel/logs/` (stale, from old runs with different CWD). Delete `internal/kernel/logs/`, ensure code only writes to `logs/` in project root, add `internal/kernel/logs/` to `.gitignore`.
- **Structured logging** -- migrate 90+ `log.Printf` calls to structured slog. Add debug logs to IPC, process, runtime subsystems. Bridge slog to EventLog for dashboard visibility.

**Plan:** `docs/plans/007-debug-logging-and-dashboard.md` (NOT implemented)

### [ ] 14. WebUI cleanup and improvements
Current WebUI has stale elements from older iterations:
- **Remove duplicate action buttons** -- "Send Task", "Run Task (Queen)", "Spawn Child", "Send Message" are 4 overlapping actions. Keep only **"Send Message"** (simplified: plain text input, not raw JSON -- user types "check weather in Florida", not `{"type":"task_request"}`). Spawn/kill are agent-level ops, not user-facing.
- **Remove Chat page** -- `chat.html` + `chat.js` are obsolete (messaging via Send Message covers this). Remove nav link too.
- **Keep** -- Kill button (useful), process tree, logs panel, cron page.
- **Future** -- task history panel, improved cron page with last_run/next_run.

**Plan:** `docs/plans/008-webui-healthcheck-demo.md` (partially obsolete, needs revision)

### [ ] 15. Mermaid architecture diagrams
Create visual documentation:
- **System architecture** -- 5-layer model, how components connect
- **Agent communication** -- IPC broker routing (parent/child/sibling/cross-branch), message flow
- **Inbox/mailbox model** -- message lifecycle, priority queue, delivery, ack
- **Cron system** -- poller -> scheduler -> executor -> agent
- **Process pipeline** -- lifecycle of a complex task: user request -> Queen -> assess -> spawn lead -> decompose -> workers -> synthesize -> result
- **Spawn chain** -- kernel -> runtime manager -> OS process -> READY handshake -> gRPC dial -> Init
- **Swarm scenario** -- web dev example: full process tree with message flow

Location: `docs/diagrams/` as `.md` files with mermaid blocks, rendered in README/docs.

### [ ] 16. TUI monitor
Terminal UI (like `htop` for agents). Process tree, message flow, resource usage, live logs. For quick monitoring without a browser.

---

## P4 -- Future

### [ ] 17. Scale testing (50-100+ agents)
Load testing with many concurrent agents. Find bottlenecks in broker, scheduler, runtime manager. Target: 50+ stable agents, 100+ with graceful degradation.

### [ ] 18. OS-level sandboxing
Current isolation is application-level (ACL, capabilities). For untrusted agents: filesystem restrictions (per-agent workspace), network policy enforcement, resource limits at OS level (cgroups/containers).

---

## Completed plans

| # | Plan | Description |
|---|------|-------------|
| 001 | Role lifecycle & Queen | Process roles, cognitive tiers, Queen dispatcher |
| 002 | System health & Queen evolution | Supervisor, Maid, lead reuse, architect routing |
| 003 | Practical agents | Assistant, GitMonitor, Coder, cron engine, chat UI |
| 004 | Architecture cleanup | Startup config, decouple kernel from Python |
| 005 | PicoClaw integration | RUNTIME_CLAW, hive mode, SyscallBridge |
| 006 | Hybrid hive channels | PicoClaw multi-channel support |
| 009 | Async agent messaging | Request-response over IPC, send_and_wait |
| 013 | AgentCore | Tool calling, agent loop, persistent memory |
| 014 | Universal ToolAgent | Config-driven agents, power tools, workspace tools |

**Research (closed):** `docs/research/agent-core-gaps.md` -- all 8 gaps (context overflow, token summarization, file/shell/web/cron tools, dynamic prompt, tool listing) resolved by plans 013+014.

## Dependency graph

```
#2 Mailbox unification ----+
#1 Context persistence ----+--> #7 Practical swarm workflow
#6 Merge queue ------------+

#9 Distributed tracing --> #4 Request/LLM tracing (extends)
#5 Logging improvements <-- #13 Debug logging (foundation)

#8 Scheduler decision (deferred until #7)
#10 DAG engine (independent, enhances #7)
#11 Frozen spec (independent, enhances #7)
#12 Reconciler (independent, enhances #7)
```
