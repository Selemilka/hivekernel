# CLAUDE.md — HiveKernel Development Guide

## Project Overview

HiveKernel is a runtime for managing swarms of LLM agents, modeled after Linux processes.
- **Core** (Go): process tree, IPC, scheduler, supervision, resource management
- **Agent runtime** (Python): LLM calls, prompts, tools — pluggable via gRPC
- **Communication**: gRPC over unix domain socket (single VPS) or TCP+TLS (multi-VPS)

The canonical specification is `HIVEKERNEL-SPEC.md` in the repo root. Always consult it for architecture decisions.

## Module & Repo

- Go module: `github.com/selemilka/hivekernel`
- GitHub: `selemilka/hivekernel`

## Development Commands

```bash
# Build
"C:\Program Files\Go\bin\go.exe" build -o bin/hivekernel.exe ./cmd/hivekernel

# Run (starts core with gRPC on port 50051)
bin\hivekernel.exe --listen :50051

# Go tests
"C:\Program Files\Go\bin\go.exe" test ./internal/... -v

# Python E2E test (requires running core)
python sdk\python\examples\test_e2e.py

# Python SDK (managed by UV)
cd sdk/python && "C:\Users\selem\.local\bin\uv.exe" sync
cd sdk/python && "C:\Users\selem\.local\bin\uv.exe" run python -c "from hivekernel_sdk import HiveAgent; print('OK')"

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

## Architecture Rules (from spec)

1. **Cognitive tier of child <= parent** (strategic > tactical > operational)
2. **Task role cannot be strategic**
3. **USER is inherited** down the branch — all children share parent's identity
4. **Daemon = system service** (queen, maid), auto-restart. **Agent = user-facing** (Leo, Shop), notify-on-crash
5. **IPC routing**: parent<->child direct, siblings through broker, cross-branch through nearest common ancestor
6. **Priority aging**: `effective = base_priority - age * aging_factor`
7. **Cron is core's job**, not agents' — queen holds the crontab

## Project Structure

```
cmd/hivekernel/main.go       — Entry point, gRPC server setup, demo scenario
internal/
  kernel/                     — King (PID 1), config, gRPC CoreService, type converters
  process/                    — Registry, spawner, signals, tree ops, supervisor
  ipc/                        — Priority queue with aging
  runtime/                    — Agent runtime manager, health monitor
  daemons/                    — Maid (per-VPS health daemon)
  scheduler/                  — (Phase 2+)
  resources/                  — (Phase 3+)
  permissions/                — (Phase 3+)
  cluster/                    — (Phase 4+)
api/proto/                    — Proto definitions + generated Go code in hivepb/
sdk/python/                   — Python SDK (UV-managed)
```

## Coding Conventions

- **Go**: standard library style, `internal/` packages, no third-party deps except gRPC/protobuf
- **Tests**: `_test.go` in same package, table-driven where appropriate, test file per source file
- **Proto**: both services share `go_package = hivepb` (single Go package)
- **Python**: relative imports within `hivekernel_sdk`, generated proto files need `from .` import fix
- **Git**: commit at each phase completion, descriptive messages
- **Console**: avoid unicode arrows/emojis in print statements (Windows cp1251 encoding)
- This project runs on Windows with PowerShell. Use PowerShell cmdlets, not Unix commands. Use Select-String instead of grep, Get-ChildItem instead of find, Get-Content instead of cat/head/tail.

## Roadmap

See `CHANGELOG.md` for completed phases and `HIVEKERNEL-SPEC.md` section "Development Roadmap" for the full plan.
