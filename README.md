# HiveKernel

A Linux-inspired kernel for orchestrating swarms of LLM agents.

Go kernel handles the process tree, IPC, scheduling, supervision, and resource management.
Agents (Python or [PicoClaw](https://github.com/sipeed/picoclaw)) make LLM calls
and do the actual work. They communicate with the kernel over gRPC.

## Documentation

### Architecture
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) -- system internals: 5-layer model, process model, IPC, resources, permissions, scheduling, runtime, cluster, gRPC API, Python SDK
- [docs/AGENT-KERNEL-INTERACTION.md](docs/AGENT-KERNEL-INTERACTION.md) -- how agents talk to the kernel: mailbox, execute_task, CoreClient, entry points, class hierarchy
- [docs/MAILBOX.md](docs/MAILBOX.md) -- mailbox system: message types, routing, priority, patterns (fire-and-forget, send-and-wait), execute_task for delegation
- [docs/AGENT-ROLES.md](docs/AGENT-ROLES.md) -- role design, cognitive tiers, lifecycle

### Plans and research
- [docs/plans/](docs/plans/) -- implementation plans (001-014, next: 015)
- [docs/research/](docs/research/) -- research notes and gap analyses

### Development
- [CLAUDE.md](CLAUDE.md) -- development guide: commands, conventions, rules
- [TODO.md](TODO.md) -- unified task tracker with priorities and dependencies
- [CHANGELOG.md](CHANGELOG.md) -- development history

## Roadmap

See [TODO.md](TODO.md) for the full prioritized task list with dependencies.

## Architecture

HiveKernel follows a 5-layer architecture inspired by operating system design:

```
  Layer 5  APPLICATION AGENTS              Queen, Assistant, Workers...
           (Python SDK / PicoClaw)         any runtime that speaks gRPC
  Layer 4  RUNTIME BRIDGE (Go)             Manager, Executor, HealthMonitor
  Layer 3  INFRASTRUCTURE (Go)             Scheduler, Cron, Cluster, Permissions, Resources
  Layer 2  CORE KERNEL PRIMITIVES (Go)     Registry, Spawner, Signals, Broker, EventLog...
  Layer 1  SYSCALL INTERFACE (gRPC proto)  10 syscalls: the contract between kernel and agents
```

**Kernel starts clean.** `bin/hivekernel.exe --listen :50051` runs a pure kernel
with no Python dependencies. Agents are optional and spawned from config:

```
bin/hivekernel.exe --listen :50051 --startup configs/startup-full.json
```

### Multi-runtime support

The kernel is runtime-agnostic. Any process that implements the AgentService gRPC
interface can be managed. Currently supported runtimes:

| Runtime | Language | Use case |
|---------|----------|----------|
| `RUNTIME_PYTHON` | Python | Full SDK with LLM agents, orchestrators, tools |
| `RUNTIME_CLAW` | Go (PicoClaw) | Lightweight agents for IoT/edge, Telegram/Discord bots |
| `RUNTIME_CUSTOM` | Any | Bring your own gRPC agent |

### Process tree (with startup-full.json)

```
  King (PID 1, Go kernel)          -- init(1), owns all subsystems
    +-- Queen (daemon, sonnet)     -- task dispatcher: complexity assessment + routing
    +-- Assistant (daemon, sonnet) -- 24/7 chat, cron scheduling, code delegation
    +-- Coder (daemon, sonnet)     -- on-demand code generation
    +-- GitMonitor (daemon, mini)  -- cron-triggered repo monitoring
    +-- Maid (daemon, mini)        -- LLM-powered health monitoring
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

### Go kernel (37 source files, 281 tests)

| Subsystem | What it does |
|-----------|-------------|
| Process tree | PID/PPID registry, 7 roles (kernel/daemon/agent/architect/lead/worker/task), 3 cognitive tiers (strategic/tactical/operational), 6 states (idle/running/blocked/sleeping/zombie/dead) |
| Spawner | Validation: child tier <= parent, role compatibility, max children, budget checks |
| Supervision | Restart policies (always/notify/never), exponential backoff, zombie reaping, SIGCHLD to parent, orphan reparenting |
| IPC | Priority queue with aging, message broker (parent/child/sibling/cross-branch routing), shared memory (4 visibility levels), pipes, pub/sub event bus |
| Resources | Token budgets per process (hierarchical allocation), rate limiter (sliding window), cgroup limits, usage accounting by user/VPS/process/tier |
| Permissions | USER identity inheritance, role-based ACL (7 actions), capability system (10 capabilities, grant/revoke per process) |
| Scheduler | Task priority with aging, tier/role matching, cron scheduling (5-field expressions, 4 action types) |
| Cluster | Node discovery, VPS connector, branch migration with rollback |
| Runtime manager | Multi-runtime spawning (Python, PicoClaw, custom), READY handshake, gRPC dial, graceful shutdown, process exit watcher |
| Health monitor | gRPC heartbeat (10s interval, 3 failures = kill), auto zombie transition |
| Event sourcing | Ring buffer (4096) + atomic sequence, JSONL disk persistence, gRPC streaming with replay-from-seq |
| Distributed tracing | trace_id/span propagation through IPC messages and execute_on chains |
| gRPC API | CoreService: spawn, kill, process info, list children, send/subscribe messages, store/get/list artifacts, execute task, resource usage, subscribe events |

### Python SDK (17 source files, 145 tests)

| Module | What it does |
|--------|-------------|
| `HiveAgent` | Base class. Lifecycle (init/shutdown), Execute bidi stream, auto-exit for task role, async mailbox (DeliverMessage) |
| `LLMAgent` | Adds async OpenRouter client. `ask(prompt)` and `chat(messages)` with model routing |
| `QueenAgent` | Task dispatcher daemon. 3-tier routing, lead reuse (idle pool with 5min timeout), heuristic + LLM complexity assessment, task history (Jaccard similarity) |
| `AssistantAgent` | 24/7 chat daemon. Answers questions, schedules cron, delegates code generation to Coder |
| `CoderAgent` | Code generation daemon. Generates code via LLM, strips markdown fences, stores as artifact |
| `GitHubMonitorAgent` | Cron-triggered daemon. Checks GitHub repos for new commits, stores state, escalates updates |
| `OrchestratorAgent` | Task decomposition into groups via LLM. Parallel group execution, sequential subtasks within group. Accepts pre-built plans from Architect |
| `WorkerAgent` | Stateful worker. `_task_memory` accumulates context across sequential tasks |
| `ArchitectAgent` | Strategic planner. Produces structured JSON plan with groups, risks, acceptance criteria. Sleeps after planning |
| `MaidAgent` | Optional health daemon. LLM-powered anomaly detection, escalation to parent |
| `SyscallContext` | In-stream syscalls: spawn, kill, send, execute_on, store/get artifact, escalate, log, report_progress, wait_child, list_siblings |
| `LLMClient` | Async OpenRouter HTTP client. Zero deps beyond stdlib |
| `CoreClient` | gRPC wrapper for CoreService |

### PicoClaw integration

[PicoClaw](https://github.com/sipeed/picoclaw) agents run as first-class kernel
processes via `RUNTIME_CLAW`. The kernel spawns PicoClaw with the `picoclaw hive`
command, which starts a gRPC AgentService server and signals readiness via the
READY protocol.

PicoClaw agents get access to kernel syscalls through a `SyscallBridge` that
translates tool calls (spawn, send, store, kill, etc.) into gRPC CoreService RPCs.
This means PicoClaw's 20+ built-in tools (shell, files, web, I2C, SPI) work
alongside kernel syscalls seamlessly.

Config example (`configs/startup-claw.json`):
```json
{
  "agents": [{
    "name": "assistant",
    "role": "daemon",
    "cognitive_tier": "tactical",
    "runtime_type": "claw",
    "claw_config": {
      "workspace": "./workspace",
      "provider": "anthropic",
      "channels": ["telegram"],
      "env": { "ANTHROPIC_API_KEY": "${ANTHROPIC_API_KEY}" }
    }
  }]
}
```

### Web dashboard

FastAPI backend + D3.js frontend. Event-driven (subscribes to kernel's
`SubscribeEvents` gRPC stream, pushes deltas over WebSocket). 30s safety resync.

Features: process tree visualization, spawn/kill/execute from UI, task history,
messages panel (send/receive with trace_id), cron management, chat interface.

### 10 syscalls

`spawn` `kill` `send` `execute_on` `store_artifact` `get_artifact` `escalate` `log` `report_progress` `wait_child`

Plus `list_siblings` for peer discovery.

## Quick start

### Prerequisites

- Go 1.24+
- Python 3.11+ with `grpcio`, `grpcio-tools`
- (Optional) PicoClaw binary for `RUNTIME_CLAW` agents

### Build and run

```bash
# Build
go build -o bin/hivekernel.exe ./cmd/hivekernel

# Start pure kernel (no Python needed)
bin/hivekernel.exe --listen :50051

# Start with Python agents
bin/hivekernel.exe --listen :50051 --startup configs/startup-full.json

# Start with PicoClaw agents
bin/hivekernel.exe --listen :50051 --startup configs/startup-claw.json --claw-bin /path/to/picoclaw

# Run tests
go test ./internal/... -v          # 281 Go tests
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

## Project stats

| | Files | Tests | Lines |
|---|-------|-------|-------|
| Go (kernel) | 37 src + 31 test | 281 | ~13,200 |
| Python (SDK) | 17 src + 8 test | 145 | ~7,500 |
| Examples/demos | 13 | -- | ~3,100 |
| Dashboard | 8 | -- | ~4,100 |
| **Total** | **114** | **426** | **~28,000** |

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
  runtime/                Manager (multi-runtime spawn), executor, health monitor
api/proto/                Protobuf definitions + generated Go code
sdk/python/
  hivekernel_sdk/         Agent SDK: base agent, LLM client, syscalls, types
  tests/                  Unit tests (8 files, 145 tests)
  examples/               Demos and E2E tests (13 files)
  dashboard/              FastAPI + D3.js web UI + chat interface
configs/                  Startup configs (empty, full, claw, telegram)
docs/                     Architecture, agent roles, mailbox design, plans
CHANGELOG.md              Development log
```

## License

MIT
