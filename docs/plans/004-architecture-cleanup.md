# Plan 004: Architecture Cleanup

**Goal:** Decouple kernel from Python agents. Kernel starts clean, agents are optional.

**Based on:** Investigation 002 (architecture layers, gray zone resolutions, roadmap R1-R6)

---

## Phase 1: Delete Go Maid (R3)

Easiest change, zero dependencies.

| # | Action | File |
|---|--------|------|
| 1 | Delete `internal/daemons/maid.go` | DELETE |
| 2 | Delete `internal/daemons/maid_test.go` (if exists) | DELETE |
| 3 | Remove any imports of `daemons` package from other files | EDIT |
| 4 | Run `go test ./internal/... -v` — all pass | VERIFY |

**Result:** `internal/daemons/` removed. Supervisor + HealthMonitor cover zombie reaping and crash detection.

---

## Phase 2: Startup Config Format (R6 + R1)

Design YAML schema, implement config loader, remove hardcoded spawnQueen().

| # | Action | File |
|---|--------|------|
| 1 | Create `internal/kernel/startup.go` — `StartupConfig` struct + YAML parser | NEW |
| 2 | Create `internal/kernel/startup_test.go` — parse tests | NEW |
| 3 | Create `configs/startup.yaml` — empty default (pure kernel) | NEW |
| 4 | Create `configs/startup-full.yaml` — queen + maid + assistant + gitmonitor + coder | NEW |
| 5 | Edit `cmd/hivekernel/main.go` — add `--startup` flag, remove `spawnQueen()` | EDIT |
| 6 | Edit `cmd/hivekernel/main.go` — load config, spawn agents from list | EDIT |
| 7 | Run `go test ./internal/... -v` — all pass | VERIFY |
| 8 | Manual test: kernel starts without `--startup` (no Python needed) | VERIFY |
| 9 | Manual test: kernel starts with `--startup configs/startup-full.yaml` | VERIFY |

**Config format:**
```yaml
agents:
  - name: queen
    role: daemon
    cognitive_tier: tactical
    model: sonnet
    runtime_type: python
    runtime_image: hivekernel_sdk.queen:QueenAgent

  - name: maid@local
    role: daemon
    cognitive_tier: operational
    runtime_type: python
    runtime_image: hivekernel_sdk.maid:MaidAgent

  - name: assistant
    role: daemon
    cognitive_tier: tactical
    model: sonnet
    runtime_type: python
    runtime_image: hivekernel_sdk.assistant:AssistantAgent

  - name: github-monitor
    role: daemon
    cognitive_tier: operational
    runtime_type: python
    runtime_image: hivekernel_sdk.github_monitor:GitHubMonitorAgent
    cron:
      - name: github-check
        expression: "*/30 * * * *"
        description: "Check GitHub repos for updates"

  - name: coder
    role: daemon
    cognitive_tier: tactical
    model: sonnet
    runtime_type: python
    runtime_image: hivekernel_sdk.coder:CoderAgent
```

**YAML library:** `gopkg.in/yaml.v3` (standard Go YAML lib) or manual parsing
to avoid deps. Decision: use `encoding/json` with a JSON config to stay zero-dep,
OR accept the single yaml dependency since it's a well-established lib.
Alternative: simple custom parser for this flat format.

---

## Phase 3: Queen Cleanup (R2)

Remove daemon spawning from Queen. She becomes a pure task dispatcher.

| # | Action | File |
|---|--------|------|
| 1 | Remove `_spawn_maid()`, `_spawn_assistant()`, `_spawn_github_monitor()`, `_spawn_coder()` | `queen.py` |
| 2 | Remove 4x `asyncio.create_task(self._spawn_*)` from `on_init()` | `queen.py` |
| 3 | Remove `_maid_pid`, `_assistant_pid`, `_gitmonitor_pid`, `_coder_pid` fields | `queen.py` |
| 4 | Keep: `_lead_reaper`, `_idle_leads`, `_task_history`, `_max_workers` | `queen.py` |
| 5 | Keep: `handle_task()`, `_assess_complexity()`, `_handle_simple/complex/architect()` | `queen.py` |
| 6 | Update docstring | `queen.py` |
| 7 | Update Queen unit tests (remove spawn expectations) | `tests/test_queen.py` |
| 8 | Run Python tests | VERIFY |

**Result:** Queen is a pure task dispatcher. All daemon spawning moved to startup config.

---

## Phase 4: PYTHONPATH Auto-Detection (R4)

Kernel finds SDK automatically relative to binary location.

| # | Action | File |
|---|--------|------|
| 1 | Add `--sdk-path` flag to main.go (optional, overrides auto-detect) | `main.go` |
| 2 | Add SDK path detection in runtime Manager: check `./sdk/python`, `../sdk/python`, exe-relative | `manager.go` |
| 3 | Set PYTHONPATH in spawned process environment automatically | `manager.go` |
| 4 | Remove manual PYTHONPATH requirement from docs | `QUICKSTART.md` |
| 5 | Manual test: kernel + agents work without manual PYTHONPATH | VERIFY |

**Detection order:**
1. `--sdk-path` flag (explicit)
2. `HIVEKERNEL_SDK_PATH` env var
3. `{exe_dir}/../sdk/python` (relative to binary)
4. `{cwd}/sdk/python` (relative to working dir)
5. Error if none found and agents are configured

---

## Phase 5: Update Docs + Commit

| # | Action | File |
|---|--------|------|
| 1 | Update QUICKSTART.md — new startup flow | EDIT |
| 2 | Update README.md — remove "known issues" (fixed), update quick start | EDIT |
| 3 | Update CHANGELOG.md | EDIT |
| 4 | Update investigation 002 status to "complete" | EDIT |
| 5 | Git commit + push | GIT |

---

## Implementation Order

```
Phase 1 (Go Maid)      ~10 min    no dependencies
Phase 2 (Config+main)  ~40 min    core change, enables Phase 3
Phase 3 (Queen)         ~15 min    depends on Phase 2
Phase 4 (PYTHONPATH)    ~20 min    independent, can parallel with 3
Phase 5 (Docs)          ~10 min    after all phases
```

## What's NOT in This Plan

- **R5 (migrate syscall)** — deferred, clustering not production-ready
- **Spec update** — HIVEKERNEL-SPEC.md will be updated separately after code stabilizes
- **Dashboard changes** — dashboard already uses gRPC, no changes needed
- **E2E test rewrite** — existing tests may need minor adjustments for new startup flow
