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

---

## Phase 3 — Resources + Permissions (completed)

**Goal:** token budgets, rate limiting, usage accounting, identity management, ACL, role capabilities.

### Added
- `internal/resources/budget.go` — Token budget management per process/model tier. Parent-to-child allocation, consumption tracking, release on death (unused returns to parent), branch usage aggregation
- `internal/resources/limits.go` — RateLimiter (sliding window per-process API call rate limiting), LimitChecker (spawn limits, context window, timeout enforcement)
- `internal/resources/accounting.go` — Usage tracking aggregated by user, VPS, PID, and model tier. Records token consumption events with timestamps
- `internal/permissions/auth.go` — USER identity resolution and inheritance validation. Kernel can assign any user, others must inherit from parent
- `internal/permissions/acl.go` — Access control lists with role-based default rules. Actions: spawn, kill, send_message, read/write_artifact, escalate, read_process. Cross-user access restricted (kernel only)
- `internal/permissions/capabilities.go` — Role-based capability system. Per-role capability sets (kernel=all, task=minimal). Per-process grant/revoke overrides. Tool validation (tasks cannot have shell_exec)

### Changed
- `internal/kernel/king.go` — Now owns BudgetManager, RateLimiter, LimitChecker, Accountant, AuthProvider, ACL, CapabilityChecker. SpawnChild validates ACL, capabilities, user inheritance, and budget before spawning. Kernel gets initial budget on bootstrap
- `internal/kernel/grpc_core.go` — SendMessage checks ACL and rate limits. StoreArtifact checks write permission. ReportMetric records token usage in accounting and budget. GetResourceUsage returns budget-aware remaining tokens. RequestResources fully implemented (allocates from parent budget)

### Tests: 87 passing (+35 new)
- Budget: set/get, allocate to child, insufficient funds, consume, exceed, release (return to parent), branch usage, tier mapping
- Limits: rate limiter allow/deny/expiry/remove/no-limit, spawn limit check, context window, timeout
- Accounting: record, user usage, VPS usage, process usage, total usage
- Auth: resolve identity, kernel assigns any user, non-kernel must inherit, same user check, is-kernel
- ACL: kernel can do anything, daemon permissions, worker spawn, task cannot spawn, task can send/read, cross-user denied/same-user allowed/kernel allowed, custom rules
- Capabilities: kernel has all, task no spawn/shell, worker can spawn, lead can kill, require, grant override, revoke override, validate tools, list capabilities

---

## Phase 4 — Multi-VPS (completed)

**Goal:** cluster node management, cross-VPS connections, branch migration, group resource limits.

### Added
- `internal/cluster/discovery.go` — NodeRegistry for VPS node tracking. NodeInfo (ID, address, capacity, health metrics). NodeStatus (online, overloaded, draining, offline). Register, Deregister, Get, List, ListOnline, UpdateHealth (auto-overload at 90% memory), SetStatus, Heartbeat, FindLeastLoaded (with exclusion), CheckStale
- `internal/cluster/connector.go` — VPS Connector for managing gRPC connections between nodes. Connect, Disconnect, GetConnection, IsConnected, ForwardMessage (with message logging). Error tracking with threshold-based unhealthy detection, RecordSuccess recovery. Connection pool lifecycle
- `internal/cluster/migration.go` — MigrationManager for process/branch migration between VPS. PrepareMigration (validate, snapshot branch), ExecuteMigration (update VPS in registry, update node counts), RollbackMigration. MigrationState tracking (pending, in_progress, completed, failed, rolled_back). ProcessSnapshot for state serialization. Guards: cannot migrate kernel, cannot migrate to same node, target must be online
- `internal/resources/cgroups.go` — CGroupManager for group resource limits by tree branch. Create (auto-includes root + descendants), Delete, Get, GetByPID, AddProcess, RemoveProcess. ConsumeTokens (collective budget enforcement), CheckSpawnAllowed (max processes per group). GroupUsage aggregation

### Changed
- `internal/kernel/king.go` — Now owns NodeRegistry, Connector, MigrationManager, CGroupManager. Registers self as cluster node on bootstrap. SpawnChild checks cgroup spawn limits and auto-adds children to parent's cgroup. Escalation handler recognizes "vps_overloaded" and triggers automatic migration to least-loaded node

### Tests: 154 passing (+57 new)
- Discovery: register/get, deregister, list, list online, update health, auto-overload detection, recovery, FindLeastLoaded (basic, exclude, skip offline, none available), CheckStale, MemoryFreePercent, LoadScore, Heartbeat, SetStatus
- Connector: connect/get, validation, disconnect, IsConnected, list, ForwardMessage (success, not connected, unhealthy), error threshold, recovery, error reset on forward, reconnect
- Migration: prepare (basic, cannot migrate kernel, same node, target offline), execute (basic, node count updates), rollback (basic, invalid state), execute twice, list/active, source draining restored, SnapshotProcess
- Cgroups: create (basic, duplicate, invalid root), delete, GetByPID, AddProcess (basic, max reached, idempotent), RemoveProcess, ConsumeTokens (basic, exceeds limit, not in group), CheckSpawnAllowed (basic, not in group), List, GetGroupUsage, Remaining, UsagePercent
