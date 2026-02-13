# HiveKernel Changelog

## Phase 0 — Proof of Concept (completed)

**Goal:** king -> queen -> worker, verify IPC works end-to-end.

### Go Core
- `api/proto/agent.proto` — AgentService + all shared enums/messages
- `api/proto/core.proto` — CoreService definition
- `api/proto/hivepb/` — generated Go gRPC code
- `internal/process/types.go` — Process, AgentRole, CognitiveTier, ProcessState
- `internal/process/registry.go` — thread-safe process table, tree traversal, NCA
- `internal/process/spawner.go` — spawn validation (cognitive tier, max_children, role compat)
- `internal/ipc/queue.go` — priority queue with aging (container/heap), TTL, blocking pop
- `internal/kernel/king.go` — PID 1 bootstrap, message loop
- `internal/kernel/config.go` — system configuration with defaults
- `internal/kernel/grpc_core.go` — CoreService gRPC server (SpawnChild, KillChild, GetProcessInfo, ListChildren, SendMessage, Escalate, Log, GetResourceUsage)
- `internal/kernel/convert.go` — proto <-> internal type converters
- `internal/runtime/manager.go` — agent runtime lifecycle (stub)
- `internal/runtime/health.go` — heartbeat monitoring, zombie detection
- `cmd/hivekernel/main.go` — entry point, gRPC server, demo scenario

### Python SDK
- `sdk/python/hivekernel_sdk/types.py` — TaskResult, Task, Message, AgentConfig
- `sdk/python/hivekernel_sdk/client.py` — CoreClient (gRPC wrapper)
- `sdk/python/hivekernel_sdk/agent.py` — HiveAgent base class (Init, Execute, Shutdown, Heartbeat, DeliverMessage)
- `sdk/python/examples/echo_worker.py` — demo agent
- `sdk/python/examples/test_e2e.py` — end-to-end integration test
- `sdk/python/pyproject.toml` — UV-managed project

### Tests: 15 passing
- Registry: CRUD, parent/child, NCA, PID auto-increment
- Spawner: kernel bootstrap, child spawn, cog tier violation, max_children, strategic task rejection, name required
- IPC queue: basic ordering, aging, TTL, blocking pop, cancel, ID generation

---

## Phase 1 — Process Tree + Supervision (completed)

**Goal:** crash detection, restart policies, zombie cleanup, tree operations.

### Added
- `internal/process/signals.go` — Signal types (SIGTERM, SIGKILL, SIGCHLD, SIGSTOP, SIGCONT, SIGHUP), signal delivery, grace period handling, parent notification
- `internal/process/tree.go` — KillBranch (bottom-up ordering), Reparent, OrphanAdoption, SubtreeVPS
- `internal/process/supervisor.go` — RestartPolicy per role (always/notify/never), exponential backoff, max restart cap, zombie reaping loop
- `internal/daemons/maid.go` — per-VPS health monitoring daemon (memory, zombies, process status)

### Tests: 27 passing (+12 new)
- Signals: SIGTERM->Blocked, SIGKILL->Dead, SIGCHLD delivery, grace period (TERM then KILL), STOP/CONT
- Tree: KillBranch (bottom-up), Reparent (+ SIGHUP), OrphanAdoption
- Supervisor: daemon auto-restart, task no-restart + parent notify, zombie reaping, max restart exceeded

---

## Phase 2 — Multi-agent + IPC (completed)

**Goal:** full IPC routing with priority aging, shared memory, broadcast events.

### Added
- `internal/ipc/broker.go` — Central message router with routing rule validation (parent<->child direct, siblings through broker with parent copy, cross-branch through NCA, tasks can only send to parent)
- `internal/ipc/priority.go` — Relationship-based priority (kernel > parent > sibling > child), combined with explicit priority
- `internal/ipc/shared_memory.go` — Artifact storage with 4 visibility levels (private, user, subtree, global), permission checks based on process tree and USER identity
- `internal/ipc/pipe.go` — Bidirectional byte channels parent<->child with backpressure, PipeRegistry for lifecycle management
- `internal/ipc/events.go` — Pub/sub broadcast EventBus with topic subscription, non-blocking delivery

### Changed
- `internal/kernel/king.go` — Now owns Broker, SharedMemory, EventBus, PipeRegistry; routes messages through broker
- `internal/kernel/grpc_core.go` — SendMessage uses broker (validates routing rules), Subscribe streams from broker queues, StoreArtifact/GetArtifact/ListArtifacts fully implemented

### Tests: 52 passing (+25 new)
- Broker: parent->child, child->parent, siblings (parent sees copy), task restricted to parent, named queues, cross-branch via NCA
- Shared memory: global/private/user/subtree visibility, list with prefix, owner delete, kernel delete, key required
- Pipes: bidirectional read/write, close, registry create/get/remove, backpressure
- Events: pub/sub, multiple subscribers, unsubscribe, no-subscriber safety, topic isolation
