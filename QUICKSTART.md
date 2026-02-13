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

27 tests covering:
- Process registry (CRUD, tree traversal, nearest common ancestor)
- Spawner validation (cognitive tier, max children, role compatibility)
- IPC priority queue (ordering, aging, TTL, blocking pop)
- Signals (SIGTERM, SIGKILL, SIGCHLD, grace period, STOP/CONT)
- Tree operations (KillBranch, Reparent, OrphanAdoption)
- Supervisor (daemon restart, task exit, zombie reaping, max restart cap)

## Run Python E2E Test

With the Go core running in another terminal:

```bash
python sdk\python\examples\test_e2e.py
```

This connects to the core via gRPC and tests:
- `GetProcessInfo` — reads king's process entry
- `ListChildren` — lists the full process tree
- `SpawnChild` — spawns a new task under queen
- `SendMessage` — sends a message through the IPC queue
- `Log` — writes a log entry via gRPC

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
internal/             Go core packages (kernel, process, ipc, runtime, daemons)
api/proto/            Protobuf definitions + generated Go code
sdk/python/           Python agent SDK
HIVEKERNEL-SPEC.md    Full project specification
CLAUDE.md             Development guide for Claude Code
CHANGELOG.md          Progress log
```
