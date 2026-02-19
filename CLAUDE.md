# CLAUDE.md -- HiveKernel Development Guide

## Project Overview

HiveKernel is a runtime for managing swarms of LLM agents, modeled after Linux processes.
- **Core** (Go): process tree, IPC, scheduler, supervision, resource management
- **Agent runtime** (Python): LLM calls, prompts, tools -- pluggable via gRPC
- **Communication**: gRPC over unix domain socket (single VPS) or TCP+TLS (multi-VPS)

Architecture docs: `docs/ARCHITECTURE.md`, `docs/AGENT-ROLES.md`, `docs/MAILBOX.md`.

## Module & Repo

- Go module: `github.com/selemilka/hivekernel`
- GitHub: `selemilka/hivekernel`

## Development Commands

```bash
# Build
"C:\Program Files\Go\bin\go.exe" build -o bin/hivekernel.exe ./cmd/hivekernel

# Run (starts core with gRPC on port 50051)
bin\hivekernel.exe --listen :50051

# Run with agents
bin\hivekernel.exe --listen :50051 --startup configs/startup-full.json

# Go tests
"C:\Program Files\Go\bin\go.exe" test ./internal/... -v

# Python tests
cd sdk/python && "C:\Users\selem\.local\bin\uv.exe" run pytest tests/ -v

# Python SDK sync
cd sdk/python && "C:\Users\selem\.local\bin\uv.exe" sync

# Dashboard
pip install fastapi uvicorn
python sdk/python/dashboard/app.py
# Open http://localhost:8080

# Proto generation (only after changing .proto files)
"C:/protoc/bin/protoc.exe" \
  --plugin=protoc-gen-go="C:/Users/selem/go/bin/protoc-gen-go.exe" \
  --plugin=protoc-gen-go-grpc="C:/Users/selem/go/bin/protoc-gen-go-grpc.exe" \
  --proto_path=api/proto \
  --go_out=api/proto/hivepb --go_opt=paths=source_relative \
  --go-grpc_out=api/proto/hivepb --go-grpc_opt=paths=source_relative \
  api/proto/agent.proto api/proto/core.proto

# Python proto generation (fix imports after!)
python -m grpc_tools.protoc \
  --proto_path=api/proto \
  --python_out=sdk/python/hivekernel_sdk \
  --grpc_python_out=sdk/python/hivekernel_sdk \
  api/proto/agent.proto api/proto/core.proto
# Then fix: `import agent_pb2` -> `from . import agent_pb2` in generated files
```

## Tool Paths (Windows 10)

- Go: `C:\Program Files\Go\bin\go.exe`
- protoc: `C:/protoc/bin/protoc.exe`
- protoc plugins: `C:/Users/selem/go/bin/protoc-gen-go.exe`, `protoc-gen-go-grpc.exe`
- UV: `C:\Users\selem\.local\bin\uv.exe`
- Python: `python` (3.13.0)

## Architecture Rules

1. **Cognitive tier of child <= parent** (strategic > tactical > operational)
2. **Task role cannot be strategic**
3. **USER is inherited** down the branch -- all children share parent's identity
4. **Daemon = system service** (queen, maid), auto-restart. **Agent = user-facing**, notify-on-crash
5. **IPC routing**: parent<->child direct, siblings through broker, cross-branch through nearest common ancestor
6. **Priority aging**: `effective = base_priority - age * aging_factor`
7. **Cron is core's job**, not agents' -- queen holds the crontab

## Project Structure

```
cmd/hivekernel/main.go        -- Entry point, gRPC server, startup config
internal/
  kernel/                      -- King (PID 1), gRPC CoreService, syscall handler, startup config
  process/                     -- Registry, spawner, supervisor, signals, tree, event log
  ipc/                         -- Broker, priority queue, shared memory, pipes, events
  runtime/                     -- Manager (multi-runtime spawn), executor, health monitor
  scheduler/                   -- Task priority, cron scheduling
  resources/                   -- Budgets, rate limiting, cgroups, accounting
  permissions/                 -- Auth, ACL, capabilities
  cluster/                     -- Node discovery, migration, cross-VPS connectors
api/proto/                     -- Protobuf definitions + generated Go code in hivepb/
sdk/python/
  hivekernel_sdk/              -- Agent SDK: base agent, LLM client, tools, memory, agent loop
  tests/                       -- Unit tests
  examples/                    -- Demos and E2E tests
  dashboard/                   -- FastAPI + D3.js web UI
configs/                       -- Startup configs (empty, full, claw, universal, telegram)
prompts/                       -- System prompt .md files for ToolAgent roles
docs/                          -- Architecture, agent roles, mailbox design
  plans/                       -- Implementation plans (created during planning mode)
  research/                    -- Research notes and gap analyses
logs/                          -- Event log JSONL files (runtime, gitignored)
```

## Coding Conventions

- **Go**: standard library style, `internal/` packages, no third-party deps except gRPC/protobuf
- **Tests**: `_test.go` in same package, table-driven where appropriate, test file per source file
- **Proto**: both services share `go_package = hivepb` (single Go package)
- **Python**: relative imports within `hivekernel_sdk`, generated proto files need `from .` import fix
- **Git**: commit at each phase completion, descriptive messages
- **Console**: avoid unicode arrows/emojis in print statements (Windows cp1251 encoding)
- This project runs on Windows with PowerShell. Use PowerShell cmdlets, not Unix commands. Use Select-String instead of grep, Get-ChildItem instead of find, Get-Content instead of cat/head/tail.
- **Logs directory**: only `logs/` in project root. Never write logs to `internal/`.

## Development Rules

### WebUI must stay in sync
After any change to kernel behavior, agent protocols, gRPC API, or dashboard backend -- update the WebUI (HTML/JS/CSS) to reflect those changes. Do not leave stale buttons, panels, or endpoints.

### Plans go to docs/plans/
All implementation plans (created during planning mode or otherwise) must be saved to `docs/plans/NNN-short-name.md` with sequential numbering. Next available: 015.

### Build + test after changes
After modifying Go or Python code, always run the relevant build/test commands to verify nothing is broken before committing.

## Roadmap

See `TODO.md`.
