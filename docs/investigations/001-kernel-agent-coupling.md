# Investigation 001: Kernel-Agent Coupling

**Date:** 2026-02-15
**Status:** Investigation complete, fix pending
**Severity:** Architectural violation

---

## Problem Statement

The Go kernel is supposed to be independent of Python agents, but in practice
`bin/hivekernel.exe` cannot start without Python + hivekernel_sdk installed.
The kernel always auto-spawns a Python Queen daemon, which cascades into 4 more
Python processes. This violates the spec's "pluggable runtime" and
"framework-agnostic" design.

## Root Cause

**Single point of coupling: `cmd/hivekernel/main.go` lines 97-104**

```go
go func() {
    time.Sleep(200 * time.Millisecond)
    if err := spawnQueen(king, cfg); err != nil {
        log.Printf("[startup] queen spawn error: %v", err)
    }
}()
```

The `spawnQueen()` function (lines 153-173) hardcodes:
```go
RuntimeImage: "hivekernel_sdk.queen:QueenAgent"
RuntimeType:  "RUNTIME_PYTHON"
```

This is the ONLY place where the kernel depends on Python. Everything else
(King, Registry, Runtime Manager, gRPC CoreService) is generic and agnostic.

## Cascade Effect

```
cmd/hivekernel/main.go        <-- HARDCODED (Go)
  +-- spawnQueen()
      +-- python -m hivekernel_sdk.runner --agent queen:QueenAgent
          +-- Queen.on_init()  <-- HARDCODED (Python)
              +-- spawn Maid           (hivekernel_sdk.maid:MaidAgent)
              +-- spawn Assistant      (hivekernel_sdk.assistant:AssistantAgent)
              +-- spawn GitMonitor     (hivekernel_sdk.github_monitor:GitHubMonitorAgent)
              +-- spawn Coder          (hivekernel_sdk.coder:CoderAgent)
              +-- add_cron github-check (*/30 * * * *)
```

Total: **1 hardcoded Go call** produces **5 Python processes** with no
configuration or opt-out mechanism.

## What the Spec Says

From HIVEKERNEL-SPEC.md:

> "Pluggable runtime via RuntimeType. SpawnRequest includes runtime_type
> (python/claw/custom). Core spawns appropriate binary. Future-proofs for
> Claw and other agent frameworks." (line 1070)

> "Agent may be sleeping or not spawned." (line 157)

The spec NEVER says "King must auto-spawn Queen at startup."
It shows Queen in the process table but doesn't specify who spawns it or when.

## What Is Clean vs. Dirty

### Clean (no changes needed)

| Component | Why |
|-----------|-----|
| `internal/kernel/king.go` | Generic SpawnChild(), no Python knowledge |
| `internal/runtime/manager.go` | Pluggable: handles any RuntimeType/RuntimeImage |
| `internal/process/registry.go` | Pure process table, no agent specifics |
| `internal/kernel/grpc_core.go` | Generic gRPC service, all RPCs work for any runtime |
| `sdk/python/hivekernel_sdk/` | Agent code is fine -- it's supposed to exist |

### Dirty (needs fixing)

| Component | Problem |
|-----------|---------|
| `cmd/hivekernel/main.go` | Hardcoded `spawnQueen()` call |
| `sdk/python/hivekernel_sdk/queen.py` | Hardcoded 4 daemon spawns in `on_init()` |

## Consequences

1. **Cannot run kernel without Python** -- if PYTHONPATH is wrong or SDK missing, kernel logs error at startup
2. **Cannot replace Python with Claw** without editing Go code
3. **No configuration** -- no way to say "start kernel without agents" or "start with different agents"
4. **Queen.on_init() is also hardcoded** -- even if we fix main.go, Queen still hardcodes Maid/Assistant/GitMonitor/Coder
5. **PYTHONPATH must be set manually** -- kernel doesn't know where SDK lives

## Queen: Spec vs. Reality

### What the Spec Says Queen Should Do

The spec defines Queen as a **per-VPS system daemon** (like local init) with these responsibilities:

| # | Responsibility | Spec Reference |
|---|----------------|----------------|
| 1 | **Crontab holder** — holds crontab per-VPS, wakes/spawns agents on schedule | Line 158, 1068 |
| 2 | **IPC router** — routes messages between sibling branches and across VPS | Line 99-100 |
| 3 | **Agent crash handler** — detects crash, notifies king, restart decision | Line 150 |
| 4 | **Escalation handler** — receives escalations from maid/agents | Line 122 |
| 5 | **Cross-VPS proxy** — proxies gRPC TCP/TLS between VPS | Line 240 |
| 6 | **Zombie cleanup** — assists maid in reaping zombies | Line 154 |
| 7 | **Local VPS init** — per-VPS initialization (like king is global init) | Line 982 |
| 8 | **Task routing** — receives tasks from king, routes to workers | Line 1016-1020 |

Key spec quotes:
> "Scheduling is a **core responsibility**, not agent's. Agent may be sleeping
> or not spawned — someone must wake/spawn it. Queen holds the crontab per VPS,
> triggers tasks on schedule." (line 158)

> "queen.go — Local init per VPS, proxies to king" (line 982)

### What Queen Actually Does in Code

| # | What Code Does | In Spec? |
|---|----------------|----------|
| 1 | **Hardcoded daemon spawning** — always spawns Maid, Assistant, GitMonitor, Coder | NO |
| 2 | **LLM-based complexity assessment** — heuristic + LLM to route tasks | NO (spec doesn't mention LLM for routing) |
| 3 | **Lead pool management** — reuses idle orchestrator leads | NO |
| 4 | **Task history** — remembers recent task results | NO |
| 5 | **Maid health integration** — reads health artifacts | Partially (spec mentions escalation path) |
| 6 | **Three execution strategies** — simple/complex/architect | NO (spec just says "queen spawns worker") |

### What Queen DOESN'T Do (but Spec Says She Should)

| # | Missing Responsibility | Where It Went |
|---|----------------------|---------------|
| 1 | **Cron/scheduling** | Moved to Go `CronScheduler` + `King.RunCronPoller()` |
| 2 | **IPC routing** | Handled by Go `Broker` |
| 3 | **Cross-VPS proxying** | Not implemented |
| 4 | **Zombie cleanup** | Handled by Go `Supervisor` |
| 5 | **Crash notification** | Handled by Go `Supervisor` restart policies |
| 6 | **Per-VPS init** | Queen doesn't do VPS-specific init |

### The Identity Crisis

Queen started as a **system-level per-VPS coordinator** in the spec (like systemd)
but evolved into an **application-level task router with LLM** (like a project manager).

The spec responsibilities were implemented in Go core instead:
- Cron → `internal/scheduler/cron.go`
- IPC routing → `internal/ipc/broker.go`
- Supervision → `internal/process/supervisor.go`
- Zombie reaping → `internal/process/supervisor.go`

This happened naturally during development: when implementing each subsystem in Go,
it was cleaner to put it in the kernel rather than delegate to a Python agent.
But Queen's docstring and role were never updated to reflect this shift.

### What Queen Actually Is Now

Queen is effectively a **task dispatcher with LLM intelligence**:

```
Receive task
  → Assess complexity (heuristic + LLM)
  → Route to execution strategy:
      simple   → spawn one-off Worker, execute, collect
      complex  → acquire Lead (orchestrator), delegate, collect
      architect → spawn Architect → get plan → Lead executes plan
  → Store result artifact
```

Plus a **daemon spawner** at startup (Maid, Assistant, GitMonitor, Coder).

These are two completely different concerns mixed together:
1. **Daemon lifecycle** (on_init: spawn children) — system concern
2. **Task routing** (handle_task: assess + dispatch) — application concern

---

## Options for Fixing

### Option A: Config File

Add `configs/startup.yaml` (or `--config` flag):

```yaml
# Empty = kernel runs alone (pure mode)
startup_agents:
  - name: queen
    role: daemon
    cognitive_tier: tactical
    model: sonnet
    runtime_type: python
    runtime_image: hivekernel_sdk.queen:QueenAgent
```

Queen would also read a config for its children instead of hardcoding them:

```yaml
# queen_agents.yaml
daemons:
  - name: maid@local
    role: daemon
    cognitive_tier: operational
    runtime_image: hivekernel_sdk.maid:MaidAgent
  - name: assistant
    role: daemon
    cognitive_tier: tactical
    model: sonnet
    runtime_image: hivekernel_sdk.assistant:AssistantAgent
    # ...
```

**Pros:** Flexible, user controls everything, spec-compliant
**Cons:** More files, more complexity

### Option B: CLI Flags

```bash
# Pure kernel (no agents)
bin/hivekernel.exe --listen :50051

# With Queen (explicit opt-in)
bin/hivekernel.exe --listen :50051 --spawn queen:python:hivekernel_sdk.queen:QueenAgent
```

**Pros:** Simple, no config files
**Cons:** Long command lines, hard to specify Queen's children

### Option C: Demo Mode Flag

```bash
# Pure kernel
bin/hivekernel.exe --listen :50051

# Full demo with all agents
bin/hivekernel.exe --listen :50051 --demo
```

`--demo` spawns Queen with hardcoded params. Default = pure kernel.

**Pros:** Simplest change, backward compatible
**Cons:** Still hardcoded, just behind a flag

### Option D: External Spawner Script

Kernel starts clean. A separate script spawns agents:

```bash
# Terminal 1: pure kernel
bin/hivekernel.exe --listen :50051

# Terminal 2: spawn agents
python sdk/python/tools/spawn_agents.py --config agents.yaml
```

**Pros:** Complete separation, kernel truly independent
**Cons:** Extra step for users, two commands instead of one

## Recommendation

**Option A (Config File)** is the most aligned with the spec and scales well.
Option C is an acceptable quick fix if we want minimal changes.

Queen's on_init() should also be made configurable (read from config or params)
instead of hardcoding 4 daemon spawns.

## Files That Would Change

| File | Change |
|------|--------|
| `cmd/hivekernel/main.go` | Remove hardcoded spawnQueen(), add config loader |
| `internal/kernel/config.go` | Add StartupAgents config struct |
| `sdk/python/hivekernel_sdk/queen.py` | Read daemon list from config/params instead of hardcoding |
| NEW: `configs/startup.yaml` | Default startup config |
| NEW: `configs/queen.yaml` | Queen's daemon config (optional) |
