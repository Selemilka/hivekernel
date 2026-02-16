# HiveKernel

A Linux-inspired runtime for managing swarms of LLM agents.

Go kernel handles the process tree, IPC, scheduling, supervision, and resource management.
Python agents make LLM calls and do the actual work. They communicate over gRPC.

## Architecture

HiveKernel follows a 5-layer architecture inspired by operating system design:

```
  Layer 5  APPLICATION AGENTS (Python)        Queen, Assistant, Coder, Workers...
  Layer 4  RUNTIME BRIDGE (Go)                Manager, Executor, HealthMonitor
  Layer 3  INFRASTRUCTURE (Go)                Scheduler, Cron, Cluster, Permissions, Resources
  Layer 2  CORE KERNEL PRIMITIVES (Go)        Registry, Spawner, Signals, Broker, EventLog...
  Layer 1  SYSCALL INTERFACE (gRPC proto)     10 syscalls: the contract between kernel and agents
```

**Kernel starts clean.** `bin/hivekernel.exe --listen :50051` runs a pure kernel
with no Python dependencies. Agents are optional and spawned from config:

```
bin/hivekernel.exe --listen :50051 --startup configs/startup-full.json
```

### Process tree (with startup-full.json)

```
  King (PID 1, Go kernel)         -- init(1), owns all subsystems
    +-- Queen (daemon, sonnet)    -- task dispatcher: complexity assessment + routing
    +-- Assistant (daemon, sonnet) -- 24/7 chat, cron scheduling, code delegation
    +-- Coder (daemon, sonnet)    -- on-demand code generation
    +-- GitMonitor (daemon, mini) -- cron-triggered repo monitoring
    +-- Maid (daemon, mini)       -- LLM-powered health monitoring
```

When Queen receives a complex task, she spawns orchestrator leads and workers:

```
  Queen
    +-- Lead (tactical, orchestrator)
          +-- Worker-0 (operational, gemini-flash)
          +-- Worker-1 (operational, gemini-flash)
          +-- Worker-2 (operational, gemini-flash)
```

**King** is init(1). Manages the process tree, routes messages, enforces budgets.
Does not know about specific agents -- just manages processes.

**Queen** is an optional task dispatcher daemon. Receives tasks, assesses complexity
(heuristic keywords + LLM fallback), picks a strategy:

- **Simple** -- single worker, one LLM call
- **Complex** -- spawns an orchestrator lead, which decomposes the task into
  grouped subtasks, runs them in parallel across a worker pool
- **Architect** -- spawns a strategic planner that produces a structured JSON
  execution plan, then hands it to a lead for execution

**Lead** (orchestrator) decomposes tasks into groups. Related subtasks go to the
same worker (sequential, shared context). Independent groups run in parallel.
Leads are reused across tasks from an idle pool.

**Workers** stay alive between subtasks and accumulate context.
Each worker remembers previous results and uses them to inform the next response.

**Maid** is an optional Python agent for LLM-powered anomaly detection.
Zombie reaping and crash detection are handled by the kernel itself
(Supervisor + HealthMonitor).

## What's implemented

### Go kernel (37 source files, 265 tests)

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

### Python SDK (17 source files, 145 tests)

| Module | What it does |
|--------|-------------|
| `HiveAgent` | Base class. Lifecycle (init/shutdown), Execute bidi stream, auto-exit for task role |
| `LLMAgent` | Adds async OpenRouter client. `ask(prompt)` and `chat(messages)` with model routing |
| `QueenAgent` | Task dispatcher daemon. 3-tier routing, lead reuse (idle pool with 5min timeout), heuristic + LLM complexity assessment, task history (Jaccard similarity) |
| `AssistantAgent` | 24/7 chat daemon. Answers questions, schedules cron, delegates code generation to Coder |
| `CoderAgent` | Code generation daemon. Generates code via LLM, strips markdown fences, stores as artifact |
| `GitHubMonitorAgent` | Cron-triggered daemon. Checks GitHub repos for new commits, stores state, escalates updates |
| `OrchestratorAgent` | Task decomposition into groups via LLM. Parallel group execution, sequential subtasks within group. Accepts pre-built plans from Architect |
| `WorkerAgent` | Stateful worker. `_task_memory` accumulates context across sequential tasks |
| `ArchitectAgent` | Strategic planner. Produces structured JSON plan with groups, risks, acceptance criteria |
| `MaidAgent` | Optional health daemon. LLM-powered anomaly detection, escalation to parent |
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
go test ./internal/... -v          # 265 Go tests
python -m pytest sdk/python/tests/ -v          # 145 Python tests
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
| Go (kernel) | 37 src + 31 test | 265 | ~14,500 |
| Python (SDK) | 17 src + 8 test | 145 | ~6,900 |
| Examples/demos | 13 | -- | ~3,500 |
| Dashboard | 7 | -- | ~3,000 |
| **Total** | **113** | **410** | **~28,000** |

## Project layout

```
cmd/hivekernel/           Entry point, gRPC server, startup config
internal/
  kernel/                 King (PID 1), gRPC CoreService, syscall handler, startup config
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
  tests/                  Unit tests (8 files, 145 tests)
  examples/               Demos and E2E tests (13 files)
  dashboard/              FastAPI + D3.js web UI + chat interface
configs/                  Startup configs (empty, full agent set)
HIVEKERNEL-SPEC.md        Full specification
CHANGELOG.md              Development log (phases 0-10 + extensions)
```

## Docs

- [HIVEKERNEL-SPEC.md](HIVEKERNEL-SPEC.md) -- full specification
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) -- system internals (5-layer model)
- [docs/QUICKSTART.md](docs/QUICKSTART.md) -- getting started
- [docs/USAGE-GUIDE.md](docs/USAGE-GUIDE.md) -- practical examples
- [docs/AGENT-ROLES.md](docs/AGENT-ROLES.md) -- role design and lifecycle
- [CHANGELOG.md](CHANGELOG.md) -- development history

## License

MIT
