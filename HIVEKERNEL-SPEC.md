# HiveKernel — Project Specification

## Executive Summary

HiveKernel — runtime для управления роями LLM-агентов, построенный по модели процессов Linux. Core на Go (process tree, IPC, scheduler, supervision, resource management), agent runtime на Python (LLM-вызовы, промпты, инструменты) — pluggable, с прицелом на подключение Claw-подобных агентов. Коммуникация между core и agent runtime — gRPC over unix domain socket.

---

## Architecture Overview

### Three-Layer Design

```
┌─────────────────────────────────────────────────────────────┐
│                     HiveKernel Core (Go)                    │
│                                                             │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌────────────┐  │
│  │  Kernel   │  │ Scheduler│  │  Message  │  │  Resource   │  │
│  │ (king)    │──│          │──│  Broker   │──│  Manager    │  │
│  └────┬─────┘  └──────────┘  └──────────┘  └────────────┘  │
│       │                                                     │
│  ┌────┴─────────────────────────────────────────────────┐   │
│  │              Process Manager                          │   │
│  │  ┌──────────┐  ┌──────────┐  ┌──────────────────┐   │   │
│  │  │ Process   │  │ Process   │  │   Supervision    │   │   │
│  │  │ Registry  │  │ Spawner   │  │   Tree           │   │   │
│  │  └──────────┘  └──────────┘  └──────────────────┘   │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                             │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌────────────┐  │
│  │  Shared   │  │Permission│  │   Agent   │  │   VPS      │  │
│  │  Memory   │  │ Manager  │  │  Runtime  │  │  Connector  │  │
│  └──────────┘  └──────────┘  └──────────┘  └────────────┘  │
│                                                             │
└─────────────────────────────────────────────────────────────┘
          │ gRPC (unix socket / TCP+TLS)
          │
   ┌──────┴───────┬────────────────┐
   │              │                │
┌──┴─────┐  ┌────┴───┐     ┌─────┴────┐
│ Python │  │ Python │     │  Claw /  │
│ Agent  │  │ Agent  │     │  Other   │
└────────┘  └────────┘     └──────────┘
```

### Target Environment

- Multiple budget VPS: 2 cores, 2 GB RAM each
- One master VPS (king), others run queen + maid + agents
- Go binary ~30-50 MB RAM for core, rest available for Python agent runtimes
- Single binary deploy per VPS, no JVM, no Docker required

---

## Core Concepts

### 1. Process Tree

All agents form a single tree of arbitrary depth. Every agent (except king) has exactly one parent. The tree grows dynamically when tasks demand it and collapses when tasks complete.

### 2. Roles

Role determines lifecycle and behavior, NOT position in the tree.

| Role | Lifecycle | Manages others? | Description |
|------|-----------|-----------------|-------------|
| `kernel` | permanent | yes | Single instance, init process, manages entire system |
| `daemon` | permanent, auto-restart | yes | System services: queen, maid |
| `agent` | permanent | yes | Long-lived agent with own identity |
| `architect` | ephemeral / sleep | no, artifacts only | Designs, produces document, sleeps |
| `lead` | long-lived | yes | Team lead: decomposes tasks, coordinates workers |
| `worker` | long-lived | limited | Executor: codes, analyzes, monitors |
| `task` | short-lived | no | Atomic task: test, check, parse |

### 3. Cognitive Tiers & Model Mapping

Cognitive tier is determined by decision complexity, NOT tree depth.

| Cognitive Tier | Model | Purpose |
|----------------|-------|---------|
| `strategic` | Opus | Planning, decomposition, arbitration, non-obvious decisions |
| `tactical` | Sonnet | Medium complexity tasks, coordination, standard decisions |
| `operational` | Mini/Haiku | Routine: monitoring, parsing, checks, formatting |

**Assignment rule:** the one setting the task must be capable of evaluating the result. Consequence: an agent can only create children with cognitive tier ≤ its own.

**Exception:** cognitive tier does NOT correlate with tree depth. A `maid` (daemon, depth 3) is tactical/sonnet, but its child `memory-monitor` (task, depth 4) is operational/mini. Meanwhile a `Frontend-Lead` (lead, depth 3) is strategic/opus. Tier follows the task, not the position.

### 4. IPC — Inter-Process Communication

#### Routing Rules

| Relationship | Mechanism | Example |
|-------------|-----------|---------|
| Parent ↔ Child | Direct pipe/socket | Leo ↔ Frontend-Lead |
| Siblings | Message queue, parent sees traffic | Frontend-Lead → Testing-Lead |
| Read artifacts | Shared memory (read-only) | Testing-Lead reads architecture.md |
| Children of siblings | Through their parent only | Testing-Lead cannot contact Lexer-dev directly |
| Different branches | Through nearest common ancestor | Shop ↔ Leo — through queen |
| Between VPS | Message queue through king | queen@vps2 ↔ queen@vps3 |

#### Priority Queue with Aging

Every message has an effective priority:

```
effective_priority = base_priority + (now - enqueued_at) * aging_factor
```

Base priority by source: kernel > parent > sibling > child.

Implemented via Go's `container/heap` with priority recalculation on each Pop().

#### Escalation Chain

Each level handles what it can, escalates only what it cannot:

```
mini (memory-monitor): "RAM 95%, PID 200"
  → sonnet (maid): "Ozon-keeper eating memory, trying restart"
    → [restart failed]
    → sonnet (queen): "need migration, don't know where"
      → opus (king): "VPS3 free, migrating Shop"
```

### 5. Resource Management

| Resource | Linux Analog | Example |
|----------|-------------|---------|
| Token budget (per model tier) | CPU time | opus: 100k/day, sonnet: 2M/day |
| Context window | RAM | 128k max per agent |
| Concurrent children | max processes | Lead can have at most 5 workers |
| API rate limit | I/O bandwidth | 60 requests/min per agent |

Budgets are inherited and shared: if Leo gets 500k opus tokens for a task, his leads share that budget among themselves. When a child dies, unused budget returns to parent.

### 6. Permissions

USER is inherited down the branch. All children of an agent work under its identity.

- Agent sees only its own files and shared artifacts
- Child inherits parent's permissions
- Access to other users' resources goes through kernel

### 7. Lifecycle & Supervision

| Event | Behavior |
|-------|----------|
| daemon crashed | kernel/parent daemon auto-restarts |
| agent crashed | queen notifies king, restart decision |
| worker/task timeout | Parent sends SIGTERM, then SIGKILL |
| task completed | Exit code + result returned to parent (like waitpid) |
| Entire task done | Lead/agent kills branch (`kill -TERM -pgid`) |
| Zombie task | Parent didn't collect result — maid/queen cleans up |

### 8. Scheduling (Cron)

Scheduling is a **core responsibility**, not agent's. Agent may be sleeping or not spawned — someone must wake/spawn it. Queen holds the crontab per VPS, triggers tasks on schedule.

---

## Process Table

### Schema

| Field | Description | Example |
|-------|------------|---------|
| PID | Unique agent ID | `120` |
| PPID | Parent ID | `11` |
| USER | Owner (inherited down branch) | `shop` |
| ROLE | Functional role | `kernel`, `daemon`, `agent`, `architect`, `lead`, `worker`, `task` |
| COG | Cognitive tier | `strategic`, `tactical`, `operational` |
| MODEL | LLM model | `opus`, `sonnet`, `mini` |
| VPS | Host machine | `vps2` |
| STATE | Current state | `running`, `sleeping`, `blocked`, `zombie` |
| CTX | Context window usage | `45k/128k` |
| TOKENS | Tokens consumed (session) | `12.4k` |
| COMMAND | Task description / prompt | `"Ozon-keeper"` |

### Full System Snapshot (reference deployment)

```
PID  PPID USER      ROLE       COG    MODEL   VPS   STATE    COMMAND
──────────────────────────────────────────────────────────────────────
1    -    root      kernel     strat  opus    vps1  running  king

10   1    root      daemon     tact   sonnet  vps1  running  queen@vps1
11   1    root      daemon     tact   sonnet  vps2  running  queen@vps2
12   1    root      daemon     tact   sonnet  vps3  running  queen@vps3

20   10   root      daemon     tact   sonnet  vps1  running  maid@vps1
21   20   root      task       oper   mini    vps1  running  memory-monitor
22   20   root      task       oper   mini    vps1  running  disk-monitor

30   11   root      daemon     tact   sonnet  vps2  running  maid@vps2
31   30   root      task       oper   mini    vps2  running  memory-monitor

40   12   root      daemon     tact   sonnet  vps3  running  maid@vps3
41   40   root      task       oper   mini    vps3  running  memory-monitor

100  10   caroline  agent      strat  opus    vps1  running  Caroline
101  10   stas      agent      strat  opus    vps1  running  Stas

110  11   dora      agent      tact   sonnet  vps2  running  Dora (twitter scout)
120  11   leo       agent      strat  opus    vps2  running  Leo (researcher)
130  120  leo       worker     tact   sonnet  vps2  running  Coder

400  120  leo       architect  strat  opus    vps2  sleeping Architect
410  120  leo       lead       strat  opus    vps2  running  Frontend-Lead
411  410  leo       worker     tact   sonnet  vps2  running  Lexer-dev
412  410  leo       worker     tact   sonnet  vps2  running  Parser-dev
413  410  leo       task       oper   mini    vps2  running  Lexer-tests
420  120  leo       lead       strat  opus    vps3  running  IR-Lead
421  420  leo       worker     tact   sonnet  vps3  running  IR-builder
422  420  leo       worker     tact   sonnet  vps3  running  Optimizer
430  120  leo       lead       strat  opus    vps3  running  Backend-Lead
431  430  leo       worker     tact   sonnet  vps3  running  Codegen-x86
432  430  leo       worker     tact   sonnet  vps3  running  Codegen-arm
440  120  leo       lead       tact   sonnet  vps2  running  Testing-Lead
441  440  leo       task       oper   mini    vps2  running  Unit-runner
442  440  leo       task       oper   mini    vps2  running  Integration-runner

500  11   shop      agent      strat  opus    vps2  running  Shop
510  500  shop      worker     tact   sonnet  vps2  running  Ozon-keeper
511  510  shop      task       oper   mini    vps2  running  price-checker
512  510  shop      task       oper   mini    vps2  running  feedback-monitor
513  510  shop      task       tact   sonnet  vps2  running  feedback-responder
520  500  shop      worker     tact   sonnet  vps2  running  WB-keeper
521  520  shop      task       oper   mini    vps2  running  price-checker
522  520  shop      task       oper   mini    vps2  running  stock-monitor
```

---

## gRPC Contract

### Transport

- Within single VPS: gRPC over unix domain socket
- Between VPS: gRPC over TCP/TLS, proxied through queen

### Two Directions

1. **Core → Agent (AgentService):** core manages the agent
2. **Agent → Core (CoreService):** agent uses system capabilities

---

### AgentService — implemented by every agent runtime

```protobuf
syntax = "proto3";
package hivekernel.agent;

service AgentService {
  // Lifecycle
  rpc Init(InitRequest) returns (InitResponse);
  rpc Shutdown(ShutdownRequest) returns (ShutdownResponse);
  rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse);

  // Execution — bidirectional stream for syscalls during execution
  rpc Execute(stream ExecuteInput) returns (stream TaskProgress);

  // Interrupt current task (SIGINT)
  rpc Interrupt(InterruptRequest) returns (InterruptResponse);

  // Receive message from another agent
  rpc DeliverMessage(AgentMessage) returns (MessageAck);
}

// ─── Lifecycle ───

message InitRequest {
  uint64 pid = 1;
  uint64 ppid = 2;
  string user = 3;
  AgentRole role = 4;
  CognitiveTier cognitive_tier = 5;
  AgentConfig config = 10;
  ResourceLimits limits = 11;
  repeated Tool tools = 12;
  map<string, string> env = 13;
}

message AgentConfig {
  string name = 1;
  string system_prompt = 2;
  string model = 3;                  // "opus", "sonnet", "mini" or specific model string
  map<string, string> metadata = 4;
}

message ResourceLimits {
  uint64 max_tokens_per_hour = 1;
  uint64 max_context_tokens = 2;
  uint32 max_children = 3;
  uint32 max_concurrent_tasks = 4;
  uint32 timeout_seconds = 5;        // 0 = no timeout
  uint64 max_tokens_total = 6;
}

message Tool {
  string name = 1;
  string description = 2;
  map<string, string> config = 3;
  repeated string allowed_paths = 4;
}

message InitResponse {
  bool ready = 1;
  string error = 2;
}

message ShutdownRequest {
  ShutdownReason reason = 1;
  uint32 grace_period_seconds = 2;
}

enum ShutdownReason {
  SHUTDOWN_NORMAL = 0;
  SHUTDOWN_PARENT_DIED = 1;
  SHUTDOWN_BUDGET_EXCEEDED = 2;
  SHUTDOWN_MIGRATION = 3;
  SHUTDOWN_USER_REQUEST = 4;
}

message ShutdownResponse {
  bytes state_snapshot = 1;
  TaskResult partial_result = 2;
}

message HeartbeatRequest {
  uint64 timestamp = 1;
}

message HeartbeatResponse {
  AgentState state = 1;
  float context_usage_percent = 2;
  uint64 tokens_consumed = 3;
  uint32 active_children = 4;
  string current_task_id = 5;
}

// ─── Execution (bidirectional streaming) ───

message ExecuteInput {
  oneof input {
    TaskRequest task = 1;
    SyscallResult syscall_result = 2;
    string user_input = 3;
  }
}

message TaskRequest {
  string task_id = 1;
  string description = 2;
  map<string, string> params = 3;
  Priority priority = 4;
  uint32 timeout_seconds = 5;
  bytes context = 6;
  string parent_task_id = 7;
}

message TaskProgress {
  string task_id = 1;
  ProgressType type = 2;
  string message = 3;
  float progress_percent = 4;       // 0-100, -1 if unknown
  TaskResult result = 5;            // only on COMPLETED/FAILED
  repeated SystemCall syscalls = 6;  // syscall requests to core
}

enum ProgressType {
  PROGRESS_UPDATE = 0;
  PROGRESS_LOG = 1;
  PROGRESS_COMPLETED = 2;
  PROGRESS_FAILED = 3;
  PROGRESS_NEEDS_INPUT = 4;
  PROGRESS_SYSCALL = 5;
}

message TaskResult {
  int32 exit_code = 1;
  string output = 2;
  map<string, string> artifacts = 3;
  map<string, string> metadata = 4;
}

message InterruptRequest {
  string task_id = 1;
  string reason = 2;
}

message InterruptResponse {
  bool acknowledged = 1;
  TaskResult partial_result = 2;
}

// ─── Message delivery ───

message AgentMessage {
  string message_id = 1;
  uint64 from_pid = 2;
  string from_name = 3;
  MessageRelation relation = 4;
  string type = 5;
  Priority priority = 6;
  bytes payload = 7;
  uint64 timestamp = 8;
  bool requires_ack = 9;
}

message MessageAck {
  string message_id = 1;
  AckStatus status = 2;
  string reply = 3;
}

enum AckStatus {
  ACK_ACCEPTED = 0;
  ACK_QUEUED = 1;
  ACK_REJECTED = 2;
  ACK_ESCALATE = 3;
}

// ─── Syscalls (embedded in Execute stream) ───

message SystemCall {
  string call_id = 1;
  oneof call {
    SpawnRequest spawn = 2;
    KillRequest kill = 3;
    SendMessageRequest send = 4;
    StoreArtifactRequest store = 5;
    GetArtifactRequest get_artifact = 6;
    EscalateRequest escalate = 7;
    LogRequest log = 8;
  }
}

message SyscallResult {
  string call_id = 1;
  oneof result {
    SpawnResponse spawn = 2;
    KillResponse kill = 3;
    SendMessageResponse send = 4;
    StoreArtifactResponse store = 5;
    GetArtifactResponse get_artifact = 6;
    EscalateResponse escalate = 7;
    LogResponse log = 8;
  }
}

// ─── Common enums ───

enum AgentRole {
  ROLE_KERNEL = 0;
  ROLE_DAEMON = 1;
  ROLE_AGENT = 2;
  ROLE_ARCHITECT = 3;
  ROLE_LEAD = 4;
  ROLE_WORKER = 5;
  ROLE_TASK = 6;
}

enum CognitiveTier {
  COG_STRATEGIC = 0;
  COG_TACTICAL = 1;
  COG_OPERATIONAL = 2;
}

enum AgentState {
  STATE_IDLE = 0;
  STATE_RUNNING = 1;
  STATE_BLOCKED = 2;
  STATE_SLEEPING = 3;
}

enum Priority {
  PRIORITY_CRITICAL = 0;
  PRIORITY_HIGH = 1;
  PRIORITY_NORMAL = 2;
  PRIORITY_LOW = 3;
}

enum MessageRelation {
  REL_SYSTEM = 0;
  REL_PARENT = 1;
  REL_CHILD = 2;
  REL_SIBLING = 3;
}
```

---

### CoreService — provided by core, called by agents

```protobuf
service CoreService {
  // Process management
  rpc SpawnChild(SpawnRequest) returns (SpawnResponse);
  rpc KillChild(KillRequest) returns (KillResponse);
  rpc GetProcessInfo(ProcessInfoRequest) returns (ProcessInfo);
  rpc ListChildren(ListChildrenRequest) returns (ListChildrenResponse);

  // IPC
  rpc SendMessage(SendMessageRequest) returns (SendMessageResponse);
  rpc Subscribe(SubscribeRequest) returns (stream AgentMessage);

  // Shared memory
  rpc StoreArtifact(StoreArtifactRequest) returns (StoreArtifactResponse);
  rpc GetArtifact(GetArtifactRequest) returns (GetArtifactResponse);
  rpc ListArtifacts(ListArtifactsRequest) returns (ListArtifactsResponse);

  // Resources
  rpc GetResourceUsage(ResourceUsageRequest) returns (ResourceUsage);
  rpc RequestResources(ResourceRequest) returns (ResourceResponse);

  // Escalation
  rpc Escalate(EscalateRequest) returns (EscalateResponse);

  // Logging
  rpc Log(LogRequest) returns (LogResponse);
  rpc ReportMetric(MetricRequest) returns (MetricResponse);
}

// ─── Process management ───

message SpawnRequest {
  string name = 1;
  AgentRole role = 2;
  CognitiveTier cognitive_tier = 3;
  string model = 4;                  // empty = auto by cognitive_tier
  string system_prompt = 5;
  repeated string tools = 6;
  ResourceLimits limits = 7;
  map<string, string> env = 8;
  string initial_task = 9;
  RuntimeType runtime_type = 10;
  string runtime_image = 11;         // for custom: path to binary/script
  Schedule schedule = 12;            // optional cron schedule
}

enum RuntimeType {
  RUNTIME_PYTHON = 0;
  RUNTIME_CLAW = 1;
  RUNTIME_CUSTOM = 2;
}

message Schedule {
  string cron = 1;                   // "0 8 * * *"
  bool keep_alive = 2;              // true = sleeping between runs, false = respawned
}

message SpawnResponse {
  bool success = 1;
  uint64 child_pid = 2;
  string error = 3;
}

message KillRequest {
  uint64 target_pid = 1;
  bool recursive = 2;
  ShutdownReason reason = 3;
  uint32 grace_period_seconds = 4;
}

message KillResponse {
  bool success = 1;
  repeated uint64 killed_pids = 2;
  string error = 3;
}

message ProcessInfoRequest {
  uint64 pid = 1;                    // 0 = self
}

message ProcessInfo {
  uint64 pid = 1;
  uint64 ppid = 2;
  string user = 3;
  string name = 4;
  AgentRole role = 5;
  CognitiveTier cognitive_tier = 6;
  string model = 7;
  AgentState state = 8;
  string vps = 9;
  uint64 tokens_consumed = 10;
  float context_usage_percent = 11;
  uint32 child_count = 12;
  uint64 started_at = 13;
  string current_task_id = 14;
}

message ListChildrenRequest {
  bool recursive = 1;
  AgentState filter_state = 2;
}

message ListChildrenResponse {
  repeated ProcessInfo children = 1;
}

// ─── IPC ───

message SendMessageRequest {
  uint64 to_pid = 1;
  string to_queue = 2;
  string type = 3;
  Priority priority = 4;
  bytes payload = 5;
  bool requires_ack = 6;
  uint32 ttl_seconds = 7;
}

message SendMessageResponse {
  bool delivered = 1;
  string message_id = 2;
  string error = 3;
}

message SubscribeRequest {
  string queue = 1;
  string type_filter = 2;
  Priority min_priority = 3;
}

// ─── Shared memory ───

message StoreArtifactRequest {
  string key = 1;
  bytes content = 2;
  string content_type = 3;
  ArtifactVisibility visibility = 4;
}

enum ArtifactVisibility {
  VIS_PRIVATE = 0;
  VIS_USER = 1;
  VIS_SUBTREE = 2;
  VIS_GLOBAL = 3;
}

message StoreArtifactResponse {
  bool success = 1;
  string artifact_id = 2;
  string error = 3;
}

message GetArtifactRequest {
  string artifact_id = 1;
  string key = 2;
}

message GetArtifactResponse {
  bool found = 1;
  bytes content = 2;
  string content_type = 3;
  uint64 stored_by_pid = 4;
  uint64 stored_at = 5;
  string error = 6;
}

message ListArtifactsRequest {
  string prefix = 1;
  uint64 stored_by_pid = 2;
}

message ListArtifactsResponse {
  repeated ArtifactMeta artifacts = 1;
}

message ArtifactMeta {
  string artifact_id = 1;
  string key = 2;
  string content_type = 3;
  uint64 size_bytes = 4;
  uint64 stored_by_pid = 5;
  uint64 stored_at = 6;
}

// ─── Resources ───

message ResourceUsageRequest {}

message ResourceUsage {
  uint64 tokens_consumed = 1;
  uint64 tokens_remaining = 2;
  uint32 children_active = 3;
  uint32 children_max = 4;
  float context_usage_percent = 5;
  uint64 uptime_seconds = 6;
}

message ResourceRequest {
  string resource_type = 1;
  uint64 amount = 2;
  string justification = 3;
}

message ResourceResponse {
  bool granted = 1;
  uint64 granted_amount = 2;
  string reason = 3;
}

// ─── Escalation ───

message EscalateRequest {
  string issue = 1;
  EscalationSeverity severity = 2;
  map<string, string> context = 3;
  bool auto_propagate = 4;
}

enum EscalationSeverity {
  ESC_INFO = 0;
  ESC_WARNING = 1;
  ESC_ERROR = 2;
  ESC_CRITICAL = 3;
}

message EscalateResponse {
  bool received = 1;
  uint64 handled_by_pid = 2;
  string response = 3;
}

// ─── Logging ───

message LogRequest {
  LogLevel level = 1;
  string message = 2;
  map<string, string> fields = 3;
}

enum LogLevel {
  LOG_DEBUG = 0;
  LOG_INFO = 1;
  LOG_WARN = 2;
  LOG_ERROR = 3;
}

message LogResponse {}

message MetricRequest {
  string name = 1;
  double value = 2;
  map<string, string> labels = 3;
}

message MetricResponse {}
```

---

## Python SDK

Thin wrapper over gRPC so agent authors don't think about transport.

```python
# hivekernel_sdk/agent.py

class HiveAgent:
    """
    Base class. Agent authors subclass and implement handle_task.
    Everything else (lifecycle, heartbeat, IPC) is handled by SDK.
    """

    # ─── Author implements these ───

    async def on_init(self, config: AgentConfig) -> None:
        """Called after initialization. Optional."""
        pass

    async def handle_task(self, task: Task) -> TaskResult:
        """Main method. Receives task, returns result."""
        raise NotImplementedError

    async def on_message(self, message: Message) -> MessageAck:
        """Handle incoming message. Optional."""
        return MessageAck(status=ACK_ACCEPTED)

    async def on_shutdown(self, reason: str) -> bytes | None:
        """Save state before shutdown. Optional."""
        return None

    # ─── SDK provides these (call CoreService) ───

    async def spawn(self, name, role, cognitive_tier, system_prompt,
                    tools=None, model=None, initial_task=None,
                    runtime="python", limits=None) -> int:
        """Spawn child agent. Returns child PID."""

    async def kill(self, pid: int, recursive: bool = True) -> list[int]:
        """Kill child agent (and its children if recursive)."""

    async def send(self, to_pid=None, to_queue=None, type="default",
                   payload=None, priority="normal",
                   requires_ack=False, ttl=0) -> str:
        """Send message. Returns message_id."""

    async def subscribe(self, queue, type_filter=None) -> AsyncIterator[Message]:
        """Subscribe to named queue."""

    async def store(self, key, content, visibility="user") -> str:
        """Store artifact in shared memory."""

    async def get_artifact(self, key: str) -> bytes | None:
        """Read artifact."""

    async def escalate(self, issue, severity="warning",
                       auto_propagate=True) -> str:
        """Escalate problem to parent."""

    async def get_resources(self) -> ResourceUsage:
        """Check remaining resources."""

    async def request_resources(self, resource_type, amount,
                                justification) -> tuple[bool, int]:
        """Request more resources from parent."""

    async def log(self, level, message, **fields):
        """Write log entry."""

    async def report_progress(self, message, percent=-1):
        """Send intermediate status."""

    # ─── Properties (read-only) ───
    @property
    def pid(self) -> int: ...
    @property
    def ppid(self) -> int: ...
    @property
    def user(self) -> str: ...
    @property
    def role(self) -> str: ...
    @property
    def config(self) -> AgentConfig: ...
```

### Example: Simple Task Agent

```python
class PriceChecker(HiveAgent):
    async def handle_task(self, task: Task) -> TaskResult:
        product_id = task.params["product_id"]
        price = await self.check_price(product_id)

        if price > task.params.get("threshold", 0):
            await self.escalate(
                f"Price anomaly: {product_id} = {price}",
                severity="warning"
            )

        return TaskResult(
            exit_code=0,
            output=f"Price for {product_id}: {price}",
            artifacts={"price_data": json.dumps({"id": product_id, "price": price})}
        )
```

### Example: Coordinator Agent

```python
class OzonKeeper(HiveAgent):
    async def on_init(self, config):
        self.product_ids = config.metadata.get("products", "").split(",")

    async def handle_task(self, task: Task) -> TaskResult:
        checker_pids = []
        for product_id in self.product_ids:
            pid = await self.spawn(
                name=f"price-checker-{product_id}",
                role="task",
                cognitive_tier="operational",
                system_prompt="Check product price and report anomalies",
                initial_task=json.dumps({"product_id": product_id, "threshold": 1000}),
            )
            checker_pids.append(pid)

        await self.report_progress(f"Spawned {len(checker_pids)} checkers")

        results = []
        async for msg in self.subscribe(queue="price-results"):
            results.append(msg.payload)
            if len(results) >= len(self.product_ids):
                break

        return TaskResult(exit_code=0, output=json.dumps(results))

    async def on_message(self, message: Message) -> MessageAck:
        if message.type == "feedback-complaint":
            await self.spawn(
                name="feedback-responder",
                role="task",
                cognitive_tier="tactical",
                model="sonnet",
                initial_task=message.payload.decode(),
            )
            return MessageAck(status=ACK_ACCEPTED)
        return MessageAck(status=ACK_ESCALATE)
```

---

## Validation & Invariants

Rules that Core ALWAYS enforces:

### Spawn Validation

| Rule | Check |
|------|-------|
| Cognitive tier of child ≤ parent | strategic > tactical > operational |
| Parent's budget sufficient | tokens_remaining >= child.limits.max_tokens |
| max_children not exceeded | children_active < limits.max_children |
| Role compatible with cognitive tier | task cannot be strategic |
| Tools available for role | task cannot have "shell_exec" |

### IPC Routing Validation

| Sender → Receiver | Allowed? | Route |
|-------------------|----------|-------|
| Parent → Child | Yes | Direct |
| Child → Parent | Yes | Direct |
| Sibling → Sibling | Yes | Through broker, parent sees |
| Agent → Child of sibling | No | Through sibling-parent |
| Agent → Different branch | Yes (limited) | Through nearest common ancestor |
| Task → Anyone except parent | No | Escalation only |

### Resource Inheritance

- Sum of children's budgets ≤ parent's budget
- Child cannot request more than parent has remaining
- On child death, unused budget returns to parent

---

## Project Structure

```
hivekernel/
├── cmd/
│   └── hivekernel/
│       └── main.go              # Entry point
├── internal/
│   ├── kernel/
│   │   ├── king.go              # Main loop, human interface, global coordination
│   │   └── config.go            # System configuration
│   ├── process/
│   │   ├── registry.go          # Process table CRUD
│   │   ├── spawner.go           # Agent creation with validation
│   │   ├── supervisor.go        # Monitoring, restart policies, zombie cleanup
│   │   ├── signals.go           # SIGTERM, SIGKILL, SIGCHLD, custom signals
│   │   └── tree.go              # Tree operations: kill branch, find ancestor, migrate
│   ├── scheduler/
│   │   ├── scheduler.go         # Ready queue, priority dispatch
│   │   └── priority.go          # Priority calculation with aging
│   ├── ipc/
│   │   ├── broker.go            # Central message router with routing rules
│   │   ├── queue.go             # Priority queue: typed messages, TTL, retry
│   │   ├── pipe.go              # Stream data parent↔child
│   │   ├── shared_memory.go     # Artifact storage with visibility/permissions
│   │   └── events.go            # Broadcast events ("VPS down", "budget exceeded")
│   ├── resources/
│   │   ├── budget.go            # Token budgets per agent/branch/model-tier
│   │   ├── limits.go            # ulimits: max children, context, rate
│   │   ├── accounting.go        # Usage tracking by user, VPS, task
│   │   └── cgroups.go           # Group limits by tree branch
│   ├── permissions/
│   │   ├── auth.go              # USER identity, inheritance
│   │   ├── acl.go               # Access control lists
│   │   └── capabilities.go      # Capabilities per role
│   ├── runtime/
│   │   ├── manager.go           # Manages agent processes (spawn OS process, connect gRPC)
│   │   └── health.go            # Heartbeat monitoring, timeout detection
│   ├── cluster/
│   │   ├── connector.go         # SSH/gRPC between VPS
│   │   ├── discovery.go         # VPS registration, healthcheck
│   │   └── migration.go         # Agent/branch migration between VPS
│   └── daemons/
│       ├── queen.go             # Local init per VPS, proxies to king
│       └── maid.go              # Healthcheck, cleanup, monitoring tasks
├── api/
│   └── proto/
│       ├── agent.proto          # AgentService definition
│       └── core.proto           # CoreService definition
├── sdk/
│   └── python/
│       ├── hivekernel_sdk/
│       │   ├── __init__.py
│       │   ├── agent.py         # HiveAgent base class
│       │   ├── types.py         # TaskResult, Message, etc.
│       │   └── client.py        # gRPC client wrapper
│       ├── setup.py
│       └── examples/
│           ├── price_checker.py
│           └── ozon_keeper.py
├── configs/
│   ├── vps.yaml                 # VPS list, resources
│   ├── agents.yaml              # Agent definitions: role, cog, model, tools
│   ├── budgets.yaml             # Token budgets per tier
│   └── permissions.yaml         # ACL rules
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

---

## Development Roadmap

### Phase 0 — Proof of Concept (1 VPS, 3 agents)

**Goal:** king → queen → one worker, verify IPC works end-to-end.

**Build:** `registry.go`, `spawner.go`, `manager.go`, `health.go`, `queue.go`, `king.go`, `queen.go`, `agent.proto`, `core.proto`, Python SDK basics.

**Scenario:** Human sends task to king → king passes to queen → queen spawns worker (sonnet) → worker executes → result returns to human.

### Phase 1 — Process Tree + Supervision

**Add:** `supervisor.go`, `tree.go`, `signals.go`, `maid.go`.

**Scenario:** Worker crashes → maid detects → queen restarts.

### Phase 2 — Multi-agent + IPC

**Add:** `broker.go`, `pipe.go`, `shared_memory.go`, `priority.go`, `events.go`.

**Scenario:** Dora → message queue → Leo → spawn Coder → result. Full IPC routing with priority aging.

### Phase 3 — Resources + Permissions

**Add:** `budget.go`, `limits.go`, `accounting.go`, `auth.go`, `acl.go`, `capabilities.go`.

**Scenario:** Worker hits token budget → escalation → king decides.

### Phase 4 — Multi-VPS

**Add:** `connector.go`, `discovery.go`, `migration.go`, `cgroups.go`.

**Scenario:** VPS2 overloaded → king migrates Leo's branch to VPS3.

### Phase 5 — Dynamic Scaling

All together. Leo spawns leads → leads spawn workers → tree grows → task completes → tree collapses. Full compiler scenario.

---

## Key Design Decisions Log

1. **Process tree, not fixed tiers.** Tree of arbitrary depth, roles and cognitive tiers determined by task, not position. (Rejected: fixed 3-tier system — didn't accommodate deep hierarchies like compiler scenario.)

2. **Go for core, Python for agent runtime.** Go: 30 MB RAM vs JVM 400 MB on 2 GB VPS, single binary deploy, built-in race detector. Python: LLM ecosystem, fast prototyping. (Rejected: Kotlin/JVM — too heavy for budget VPS. Considered but deferred: Elixir — perfect model but poor LLM ecosystem.)

3. **gRPC over unix socket.** Typed contracts, bidirectional streaming for syscalls during execution, code generation for both Go and Python. (Alternative: REST — rejected, no streaming. Alternative: direct function calls — rejected, no language boundary.)

4. **Bidirectional streaming for Execute.** Agent can make syscalls (spawn children, send messages) during task execution without breaking the stream. Core responds in the same stream.

5. **Priority aging for message queues.** `effective_priority = base_priority + age * aging_factor`. Prevents message starvation. Base priority: kernel > parent > sibling > child.

6. **Cognitive tier ≠ tree depth.** maid (depth 3, sonnet) vs Frontend-Lead (depth 3, opus). Tier follows task complexity, not hierarchy.

7. **USER inheritance.** All children work under parent's identity. price-checker spawned by Ozon-keeper has `shop` user, cannot access `caroline` files.

8. **Cron is core responsibility.** Agents may be sleeping/unspawned. Queen holds crontab, wakes or spawns agents on schedule.

9. **Pluggable runtime via RuntimeType.** SpawnRequest includes runtime_type (python/claw/custom). Core spawns appropriate binary. Future-proofs for Claw and other agent frameworks.
