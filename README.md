# HiveKernel

A Linux-inspired runtime for managing swarms of LLM agents.

Go kernel handles the process tree, IPC, scheduling, supervision, and resource management.
Python agents make LLM calls and do the actual work. They communicate over gRPC.

## Architecture

```
                        King (PID 1, Go kernel)
                              |
                        Queen (PID 2, sonnet)
                       /      |       \
                  Maid      Lead     Architect
               (health)   (sonnet)   (sonnet)
                          /      \
                     Worker-1  Worker-2
                     (flash)   (flash)
```

**King** is PID 1. Manages the process tree, routes messages, enforces budgets.
Spawns Queen on startup.

**Queen** is the local coordinator daemon. Receives tasks, assesses complexity
(heuristic keywords + LLM fallback), picks a strategy:

- **Simple** -- single worker, one LLM call
- **Complex** -- spawns an orchestrator lead, which decomposes the task into
  grouped subtasks, runs them in parallel across a worker pool
- **Architect** -- spawns a strategic planner that produces a structured JSON
  execution plan, then hands it to a lead for execution

**Lead** (orchestrator) decomposes tasks into groups. Related subtasks go to the
same worker (sequential, shared context). Independent groups run in parallel.
Leads are reused across tasks from an idle pool.

**Workers** are `role=worker` (not task), so they stay alive between subtasks
and accumulate context. Each worker remembers previous results and uses them
to inform the next response.

**Maid** is a health daemon. Periodically scans for zombies and orphans,
escalates anomalies to King.

**Architect** produces a `{groups, analysis, risks, acceptance_criteria}` JSON
plan. The orchestrator uses it directly instead of calling LLM for decomposition.

## What's implemented

### Go kernel (37 source files, 248 tests)

| Subsystem | What it does |
|-----------|-------------|
| Process tree | PID/PPID registry, roles (kernel/daemon/agent/architect/lead/worker/task), cognitive tiers (strategic/tactical/operational), states (idle/running/blocked/sleeping/zombie/dead) |
| Spawner | Validation: child tier <= parent, role compatibility, max children, budget checks |
| Supervision | Daemon auto-restart, zombie reaping (configurable timeout), SIGCHLD to parent |
| IPC | Priority queue with aging, message broker (parent/child/sibling/cross-branch routing), shared memory (4 visibility levels), pipes, pub/sub events |
| Resources | Token budgets per process, rate limiter (sliding window), cgroup limits, usage accounting by user/VPS/process/tier |
| Permissions | USER identity inheritance, role-based ACL, capability system (grant/revoke per process) |
| Scheduler | Task priority with aging, tier/role matching, cron scheduling |
| Cluster | Node discovery, VPS connector, branch migration with rollback |
| Runtime manager | Real `os/exec` Python process spawning, READY handshake, gRPC dial, graceful shutdown, exit watcher (`cmd.Wait` goroutine for instant death detection) |
| Event sourcing | Ring buffer + atomic sequence, JSONL disk persistence (`logs/events-*.jsonl`), gRPC `SubscribeEvents` streaming with replay-from-seq |
| gRPC API | CoreService: spawn, kill, process info, list children, send message, store/get/list artifacts, execute task, resource usage, subscribe events |

### Python SDK (14 source files, 104 tests)

| Module | What it does |
|--------|-------------|
| `HiveAgent` | Base class. Lifecycle (init/shutdown), Execute bidi stream, auto-exit for task role |
| `LLMAgent` | Adds async OpenRouter client. `ask(prompt)` and `chat(messages)` with model routing |
| `QueenAgent` | Coordinator daemon. 3-tier routing, lead reuse (idle pool with 5min timeout), heuristic + LLM complexity assessment, task history (Jaccard similarity), Maid integration |
| `OrchestratorAgent` | Task decomposition into groups via LLM. Parallel group execution, sequential subtasks within group. Accepts pre-built plans from Architect |
| `WorkerAgent` | Stateful worker. `_task_memory` accumulates context across sequential tasks |
| `ArchitectAgent` | Strategic planner. Produces structured JSON plan with groups, risks, acceptance criteria |
| `MaidAgent` | Health daemon. Zombie/orphan detection, escalation to King |
| `SyscallContext` | In-stream syscalls: spawn, kill, send, execute_on, store/get artifact, escalate, log, report_progress, wait_child |
| `LLMClient` | Async OpenRouter HTTP client. Zero deps beyond stdlib (`urllib` + `asyncio.to_thread`) |
| `CoreClient` | gRPC wrapper for CoreService |

### Web dashboard

FastAPI backend + D3.js frontend. Event-driven (subscribes to kernel's
`SubscribeEvents` gRPC stream, pushes deltas over WebSocket). 30s safety resync.

### 10 syscalls

`spawn` `kill` `send` `execute_on` `store_artifact` `get_artifact` `escalate` `log` `report_progress` `wait_child`

## Quick start

### Prerequisites

- Go 1.24+
- Python 3.11+ with `grpcio`, `grpcio-tools`

### Build and run

```bash
# Build
go build -o bin/hivekernel.exe ./cmd/hivekernel

# Start pure kernel (no Python needed)
bin/hivekernel.exe --listen :50051

# Start with agents (Python SDK auto-detected)
bin/hivekernel.exe --listen :50051 --startup configs/startup-full.json

# Run tests
go test ./internal/... -v          # 248+ Go tests
python sdk/python/tests/test_queen.py -v       # 43 tests
python sdk/python/tests/test_orchestrator.py -v # 36 tests
python sdk/python/tests/test_architect.py -v    # 8 tests
python sdk/python/tests/test_maid.py -v         # 14 tests
```

### LLM demo (real AI calls)

```bash
# Create .env in project root
echo "OPENROUTER_API_KEY=sk-or-v1-your-key" > .env

# Research team: lead (sonnet) + 3 workers (gemini-flash)
python sdk/python/examples/llm_research_team.py

# Full Plan 002 E2E: tests all 5 agent roles
python sdk/python/examples/test_plan002_e2e.py
```

### Dashboard

```bash
pip install fastapi uvicorn
python sdk/python/dashboard/app.py
# Open http://localhost:8080
```

## Writing an agent

```python
from hivekernel_sdk import LLMAgent, TaskResult

class MyAgent(LLMAgent):
    async def handle_task(self, task, ctx) -> TaskResult:
        # Spawn a child
        child = await ctx.spawn(
            name="helper", role="task", cognitive_tier="operational",
            model="mini", runtime_image="my_module:HelperAgent",
            runtime_type="python",
        )
        # Delegate work
        result = await ctx.execute_on(child, "research quantum computing")
        # Store artifact
        await ctx.store_artifact(key="report", content=result.output.encode())
        return TaskResult(exit_code=0, output=result.output)
```

Available syscalls in `ctx`:

| Syscall | What it does |
|---------|-------------|
| `spawn(name, role, ...)` | Create child process (real Python agent) |
| `kill(pid)` | Terminate a child |
| `execute_on(pid, description, params)` | Delegate task to child, await result |
| `send(target_pid, type, payload)` | Send IPC message |
| `store_artifact(key, content)` | Store in shared memory |
| `get_artifact(key)` | Retrieve from shared memory |
| `escalate(severity, issue)` | Escalate to parent |
| `log(level, message)` | Write to process log |
| `report_progress(message, percent)` | Report task progress |
| `wait_child(pid)` | Wait for child to finish (like `waitpid`) |

## Project stats

| | Files | Tests | Lines |
|---|-------|-------|-------|
| Go (kernel) | 37 src + 30 test | 248 | ~20,400 |
| Python (SDK) | 14 src + 5 test | 104 | ~2,400 |
| Examples/demos | 11 | -- | -- |
| Dashboard | 3 | -- | -- |
| **Total** | **100** | **352** | **~22,800** |

## Project layout

```
cmd/hivekernel/           Entry point, gRPC server, Queen spawn
internal/
  kernel/                 King (PID 1), gRPC CoreService, syscall handler
  process/                Registry, spawner, supervisor, signals, tree, event log
  ipc/                    Broker, priority queue, shared memory, pipes, events
  resources/              Token budgets, rate limiter, accounting, cgroups
  permissions/            Auth, ACL, role capabilities
  cluster/                Node discovery, VPS connector, branch migration
  scheduler/              Task priority, scheduler, cron
  runtime/                Manager (os/exec spawn), executor, health monitor
api/proto/                Protobuf definitions + generated Go code
sdk/python/
  hivekernel_sdk/         Agent SDK: base agent, LLM client, syscalls, types
  tests/                  Unit tests (queen, orchestrator, architect, maid, agent)
  examples/               Demos and E2E tests
  dashboard/              FastAPI + D3.js web UI
HIVEKERNEL-SPEC.md        Full specification
ARCHITECTURE.md           Architecture documentation
QUICKSTART.md             Getting started guide
CHANGELOG.md              Development log (phases 0-10 + extensions)
```

## Docs

- [HIVEKERNEL-SPEC.md](HIVEKERNEL-SPEC.md) -- full specification
- [ARCHITECTURE.md](ARCHITECTURE.md) -- system internals
- [QUICKSTART.md](QUICKSTART.md) -- getting started
- [CHANGELOG.md](CHANGELOG.md) -- development history
- [USAGE-GUIDE.md](USAGE-GUIDE.md) -- practical examples
- [docs/AGENT-ROLES.md](docs/AGENT-ROLES.md) -- role design

## License

MIT
