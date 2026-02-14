# HiveKernel Quickstart

> **See also**: [ARCHITECTURE.md](ARCHITECTURE.md) for full system internals,
> [USAGE-GUIDE.md](USAGE-GUIDE.md) for practical "How do I..." examples.

## Prerequisites

- **Go 1.24+** — [go.dev/dl](https://go.dev/dl/)
- **Python 3.11+** — with `grpcio` and `grpcio-tools`
- **UV** (optional, recommended) — `powershell -c "irm https://astral.sh/uv/install.ps1 | iex"`

## Build & Run

```bash
# Clone and enter the project
cd HiveKernel

# Build the Go binary
go build -o bin/hivekernel.exe ./cmd/hivekernel

# Start the core (gRPC server on port 50051)
bin\hivekernel.exe --listen :50051
```

You should see:
```
HiveKernel starting on node vps1
[king] bootstrapped as PID 1 on vps1
[grpc] CoreService listening on :50051
[king] spawned queen@vps1 (PID 2) under PID 1, role=daemon, cog=tactical
[king] spawned demo-worker (PID 3) under PID 2, role=worker, cog=tactical
[demo] Phase 0 scenario ready: king(PID 1) -> queen(PID 2) -> worker(PID 3)
```

The demo automatically spawns king -> queen -> worker and prints the process table.

## Run Go Tests

```bash
go test ./internal/... -v
```

All Go tests passing, covering:
- Process registry (CRUD, tree traversal, nearest common ancestor)
- Spawner validation (cognitive tier, max children, role compatibility)
- IPC priority queue (ordering, aging, TTL, blocking pop)
- Signals (SIGTERM, SIGKILL, SIGCHLD, grace period, STOP/CONT)
- Tree operations (KillBranch, Reparent, OrphanAdoption)
- Supervisor (daemon restart, task exit, zombie reaping, max restart cap)
- Message broker (routing rules, sibling copy, cross-branch via NCA)
- Shared memory (4 visibility levels, list, delete, kernel override)
- Pipes (bidirectional, backpressure, registry)
- Events (pub/sub, topic isolation, unsubscribe)
- Token budgets (allocate, consume, release, branch usage)
- Rate limiter (sliding window, expiry)
- Limits (spawn, context window, timeout)
- Accounting (usage by user, VPS, process, tier)
- Auth (identity, inheritance, kernel override)
- ACL (role-based, cross-user, custom rules)
- Capabilities (role-based, grant/revoke, tool validation)
- Node discovery (register, deregister, health, FindLeastLoaded, stale detection)
- VPS connector (connect, disconnect, forward messages, error tracking, recovery)
- Branch migration (prepare, execute, rollback, node count updates, snapshot)
- Cgroups (create, delete, add/remove process, token limits, spawn limits, usage)
- **Task priority** (tier/role scoring, aging, ready queue ordering)
- **Task scheduler** (submit, assign, complete, cancel, resubmit, tier/role matching)
- **Cron scheduling** (parse, match, interval, multi-value, due detection, per-VPS)
- **Lifecycle** (sleep/wake, CompleteTask, WaitResult, CollapseBranch, ActiveChildren)
- **Compiler scenario** (full Leo pipeline: spawn leads -> workers -> tasks complete -> tree collapse)
- **Executor** (Execute stream caller: simple complete, syscall round-trip, failure, dial error)
- **Syscall handler** (in-stream dispatch: spawn, kill, send, store/get artifact, escalate, log, execute_on)
- **Runtime manager** (virtual process registration, start/stop lifecycle, client access)
- **execute_on** (parent-to-child task delegation, permission checks, no-runtime handling)

## Run Python Integration Test

With the Go core running in another terminal:

```bash
python sdk\python\examples\test_integration.py
```

31 checks covering the full Python SDK <-> Go Core pipeline:
- Process info (GetProcessInfo, pid=0 self-resolve)
- Process tree (ListChildren recursive, demo queen + worker)
- Spawn & Kill (SpawnChild, verify attributes, KillChild)
- IPC messages (parent<->child, named queues)
- Shared memory (store artifact, get by key, list with prefix)
- Resource usage (tokens, children count/max)
- Metrics & accounting (report token consumption)
- Escalation (escalate to parent)
- Logging (info, warn levels)
- ACL enforcement (task cannot spawn children)

Expected output:
```
============================================================
HiveKernel Integration Test (Phase 0-3)
Connecting to Go core at localhost:50051...
============================================================
  [PASS] king exists
  [PASS] king name
  ...
============================================================
ALL 31 CHECKS PASSED!
============================================================
```

## Python SDK Setup (for agent development)

```bash
cd sdk\python

# With UV (recommended):
uv sync
uv run python -c "from hivekernel_sdk import HiveAgent; print('OK')"

# Or with pip:
pip install -e .
```

## Writing an Agent

The kernel automatically spawns Python agent processes. Set `runtime_image` when spawning
to specify the Python class to run:

```python
from hivekernel_sdk import HiveAgent, TaskResult, AgentConfig

class MyAgent(HiveAgent):
    async def on_init(self, config: AgentConfig) -> None:
        pass  # Setup code here

    async def handle_task(self, task, ctx) -> TaskResult:
        # Spawn a child agent (real Python process)
        child_pid = await ctx.spawn(
            name="worker", role="task", cognitive_tier="operational",
            runtime_image="my_module:WorkerAgent", runtime_type="python",
        )
        # Delegate a task to the child
        result = await ctx.execute_on(pid=child_pid, description="do something")
        # Store result as shared artifact
        await ctx.store_artifact(key="result", content=result.output.encode())
        # Cleanup
        await ctx.kill(child_pid)
        return TaskResult(exit_code=0, output="done")
```

Available syscalls in `ctx`: `spawn`, `kill`, `send`, `execute_on`, `store_artifact`,
`get_artifact`, `escalate`, `log`, `report_progress`.

## Run the Runtime E2E Test (auto-spawn)

This test validates the full kernel -> Python agent lifecycle:

```bash
python sdk\python\examples\test_runtime_e2e.py
```

10 checks: kernel starts, spawns real Python agent (SubWorker), verifies process info,
connects to agent directly, executes task via bidi stream, verifies result, kills agent.

## Run the Multi-Agent Team E2E Test

This test validates hierarchical multi-agent execution:

```bash
python sdk\python\examples\test_team_e2e.py
```

13 checks: kernel spawns TeamManager, TeamManager spawns 2 SubWorkers via `ctx.spawn()`,
delegates 3 subtasks via `ctx.execute_on()`, stores results as artifact, kills workers.
All 5 processes are real Python processes communicating via gRPC.

## Run the AI Research Team Demo (one command)

The showcase demo starts the kernel, spawns a lead agent + 3 workers,
delegates research subtopics, stores artifacts, and prints results:

```bash
python sdk/python/examples/demo_showcase.py
```

Expected output shows 8 sections: process table, agent spawning, task execution,
artifacts, report summary, and cleanup. See [USAGE-GUIDE.md](USAGE-GUIDE.md)
for a full walkthrough of building similar scenarios.

## Web Dashboard

Interactive browser UI for viewing the process tree, spawning/killing agents,
executing tasks, and inspecting artifacts.

```bash
# Terminal 1: start the kernel
bin\hivekernel.exe --listen :50051

# Terminal 2: install dashboard deps & start
pip install fastapi uvicorn
python sdk/python/dashboard/app.py
```

Open http://localhost:8080 in your browser. Features:
- **Live process tree** (D3.js) with real-time delta updates via event stream
- **Event sourcing** — dashboard subscribes to kernel's `SubscribeEvents` gRPC stream, no polling
- **Disk persistence** — all events logged to `logs/events-*.jsonl` for post-mortem analysis
- **Click** a node to view details, **right-click** for context menu
- **Spawn/Kill/Execute** agents from the detail panel
- **Artifacts panel** to inspect shared memory

Environment variables: `HIVEKERNEL_ADDR` (default `localhost:50051`),
`DASHBOARD_PORT` (default `8080`).

## LLM Integration (Real AI Agents)

HiveKernel agents can make real LLM calls via OpenRouter.

### Setup

1. Get an API key from [openrouter.ai](https://openrouter.ai/)
2. Create a `.env` file in the project root:
   ```
   OPENROUTER_API_KEY=sk-or-v1-your-key-here
   ```

### Run the LLM demo

```bash
# With web dashboard (auto-opens browser, step-by-step)
python sdk/python/examples/llm_research_team.py

# Without dashboard (runs straight through)
python sdk/python/examples/llm_research_team.py --no-dashboard
```

This spawns a research lead (sonnet) + 3 workers (gemini-flash), makes real
LLM calls to break down a topic, research subtopics, and synthesize a report.
Takes ~30-60 seconds depending on API latency.

With the dashboard you can watch agents spawn and work in real-time in the
D3.js process tree. The demo pauses at key moments so you can observe.

### Use LLMAgent in your own agents

```python
from hivekernel_sdk import LLMAgent, TaskResult

class MyAgent(LLMAgent):
    async def handle_task(self, task, ctx) -> TaskResult:
        answer = await self.ask("What is 2+2?")
        return TaskResult(exit_code=0, output=answer)
```

`LLMAgent` auto-creates an OpenRouter client from `OPENROUTER_API_KEY` env var
and the agent's `model` config. See [USAGE-GUIDE.md](USAGE-GUIDE.md) for details.

## Run the Echo Worker Demo (manual)

```bash
# Terminal 1: start core
bin\hivekernel.exe --listen :50051

# Terminal 2: start agent
cd sdk\python
uv run python examples\echo_worker.py --port 50100 --core localhost:50051
```

## Project Layout

```
cmd/hivekernel/       Go entry point
internal/
  kernel/             King (PID 1), config, gRPC CoreService
  process/            Registry, spawner, supervisor, signals, tree
  ipc/                Broker, priority queue, shared memory, pipes, events
  resources/          Token budgets, rate limiter, accounting, cgroups
  permissions/        Auth (USER identity), ACL, role capabilities
  cluster/            Node discovery, VPS connector, branch migration
  scheduler/          Task priority, scheduler, cron scheduling
  runtime/            Agent runtime manager (OS process spawning), executor, health
  daemons/            Maid health daemon
api/proto/            Protobuf definitions + generated Go code
sdk/python/           Async Python agent SDK (HiveAgent, CoreClient, SyscallContext)
sdk/python/dashboard/ Web dashboard (FastAPI + D3.js)
HIVEKERNEL-SPEC.md    Full project specification
ARCHITECTURE.md       Full architecture documentation (10 sections, API ref)
USAGE-GUIDE.md        Practical "How do I..." guide with examples
CLAUDE.md             Development guide for Claude Code
CHANGELOG.md          Progress log
```
