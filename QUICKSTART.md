# HiveKernel Quickstart

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

221 tests covering:
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
- **Syscall handler** (in-stream dispatch: spawn, kill, send, store/get artifact, escalate, log)

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

## Run the Echo Worker Demo

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
  runtime/            Agent runtime lifecycle, Execute stream executor
  daemons/            Maid health daemon
api/proto/            Protobuf definitions + generated Go code
sdk/python/           Async Python agent SDK (HiveAgent, CoreClient, SyscallContext)
HIVEKERNEL-SPEC.md    Full project specification
CLAUDE.md             Development guide for Claude Code
CHANGELOG.md          Progress log
```
