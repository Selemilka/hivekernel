# HiveKernel Usage Guide

A practical "How do I..." guide for building multi-agent systems with HiveKernel.

**Prerequisites**: HiveKernel built and tests passing.
See [QUICKSTART.md](QUICKSTART.md) for setup, [ARCHITECTURE.md](ARCHITECTURE.md) for internals.

---

## Table of Contents

1. [Getting Started](#1-getting-started)
2. [Create a Simple Agent](#2-create-a-simple-agent)
3. [Spawn Child Agents](#3-spawn-child-agents)
4. [Delegate Tasks](#4-delegate-tasks-execute_on)
5. [Share Artifacts](#5-share-artifacts)
6. [Send Messages](#6-send-messages)
7. [Handle Escalations](#7-handle-escalations)
8. [Monitor Resources](#8-monitor-resources)
9. [Build a Team Scenario](#9-build-a-team-scenario)
10. [Call From External Systems](#10-call-from-external-systems)
11. [Run Kernel Programmatically](#11-run-kernel-programmatically)
12. [LLM Integration](#12-llm-integration)
13. [Troubleshooting](#13-troubleshooting)

---

## 1. Getting Started

### Build the kernel

```bash
go build -o bin/hivekernel.exe ./cmd/hivekernel
```

### Start the kernel

```bash
bin\hivekernel.exe --listen :50051
```

The kernel bootstraps two built-in processes:
- **king** (PID 1) — the root process, strategic tier
- **queen** (PID 2) — system daemon, tactical tier

### Verify with Go tests

```bash
go test ./internal/... -v
```

### Install the Python SDK

```bash
cd sdk\python
uv sync                    # or: pip install -e .
python -c "from hivekernel_sdk import HiveAgent; print('OK')"
```

### Run the demo

```bash
python sdk/python/examples/demo_showcase.py
```

This runs the full AI Research Team demo (start kernel, spawn 4 agents,
delegate 3 tasks, store artifacts, cleanup) in one command.

---

## 2. Create a Simple Agent

Every agent is a Python class that extends `HiveAgent`.

### Minimal agent

```python
# my_agent.py
from hivekernel_sdk import HiveAgent, TaskResult, AgentConfig

class MyAgent(HiveAgent):
    async def handle_task(self, task, ctx) -> TaskResult:
        return TaskResult(
            exit_code=0,
            output=f"Hello from {self.pid}! Task: {task.description}",
        )
```

That's it. The kernel spawns this as a real Python process, calls `Init` via
gRPC, then sends tasks through the bidirectional `Execute` stream.

### Four overridable methods

| Method | When called | Purpose |
|--------|-------------|---------|
| `on_init(config)` | After gRPC Init | Setup state, load models |
| `handle_task(task, ctx)` | When a task arrives | Main work, return `TaskResult` |
| `on_message(message)` | IPC message received | Handle inter-agent messages |
| `on_shutdown(reason)` | Before process dies | Save state, return snapshot bytes |

### Agent properties (read-only)

```python
self.pid        # int — this agent's PID
self.ppid       # int — parent's PID
self.user       # str — inherited user identity
self.role       # str — "lead", "worker", "task", etc.
self.config     # AgentConfig — name, system_prompt, model, metadata
self.core       # CoreClient — direct gRPC access (for on_init/on_shutdown)
```

### Task and TaskResult

```python
@dataclass
class Task:
    task_id: str            # unique task identifier
    description: str        # human-readable description
    params: dict[str, str]  # key-value parameters
    timeout_seconds: int    # 0 = no timeout
    context: bytes          # binary context (e.g. serialized state)
    parent_task_id: str     # set if delegated from parent task

@dataclass
class TaskResult:
    exit_code: int              # 0 = success
    output: str                 # text output
    artifacts: dict[str, str]   # key-value artifact references
    metadata: dict[str, str]    # key-value metadata
```

### runtime_image format

When spawning an agent, the `runtime_image` string tells the kernel which
Python class to load:

```
"module_name:ClassName"
```

Examples:
- `"my_agent:MyAgent"` — loads `my_agent.py`, class `MyAgent`
- `"agents.researcher:ResearchLead"` — loads `agents/researcher.py`, class `ResearchLead`
- `"examples.demo_showcase:ResearchWorker"` — from SDK examples

The kernel runs: `python -m hivekernel_sdk.runner --agent <image> --core <addr>`

Make sure the module is importable (i.e. on `PYTHONPATH`).

---

## 3. Spawn Child Agents

Use `ctx.spawn()` inside `handle_task()` to create child agents.

### Basic spawn

```python
async def handle_task(self, task, ctx) -> TaskResult:
    child_pid = await ctx.spawn(
        name="my-worker",
        role="task",
        cognitive_tier="operational",
        runtime_image="my_module:WorkerAgent",
        runtime_type="python",
    )
    # child_pid is an int (e.g. 5)
    return TaskResult(exit_code=0, output=f"Spawned PID {child_pid}")
```

### ctx.spawn() parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `name` | str | yes | Process name (must be non-empty) |
| `role` | str | yes | "kernel", "daemon", "agent", "architect", "lead", "worker", "task" |
| `cognitive_tier` | str | yes | "strategic", "tactical", "operational" |
| `model` | str | no | LLM model hint ("opus", "sonnet", "mini") |
| `system_prompt` | str | no | Agent's system prompt |
| `tools` | list[str] | no | Allowed tool names |
| `runtime_image` | str | no | Python class to spawn ("module:Class") |
| `runtime_type` | str | no | "python" (default), "claw", "custom" |
| `initial_task` | str | no | Task description to queue on spawn |
| `limits` | dict | no | Resource limits (max_tokens, etc.) |

### Spawn rules

The kernel enforces these constraints:

1. **Parent must be alive**: dead or zombie processes cannot spawn children

2. **Cognitive tier**: child tier <= parent tier
   - strategic parent can spawn any tier
   - tactical parent can spawn tactical or operational
   - operational parent can spawn only operational

3. **Role restrictions**: task role cannot be strategic tier

4. **Max children**: default 10 per process (configurable via limits)

5. **Name required**: empty name is rejected

### Spawn multiple workers

```python
worker_pids = []
for i in range(3):
    pid = await ctx.spawn(
        name=f"worker-{i}",
        role="task",
        cognitive_tier="operational",
        runtime_image="my_module:WorkerAgent",
        runtime_type="python",
    )
    worker_pids.append(pid)
```

### Kill children

```python
# Kill one child (and its subtree)
killed = await ctx.kill(child_pid)  # returns list of killed PIDs

# Kill with recursive=False (only the target, not subtree)
killed = await ctx.kill(child_pid, recursive=False)
```

Killed processes enter **zombie** state (not immediately removed). The supervisor
reaps zombies after 60 seconds, or the parent can collect results explicitly
with `wait_child`.

### Wait for child to exit (like waitpid)

```python
# Wait for a child to finish (blocks until zombie/dead or timeout)
info = await ctx.wait_child(child_pid, timeout_seconds=30)
print(f"PID {info['pid']} exited with code {info['exit_code']}")
print(f"Output: {info['output']}")
```

This is useful for fire-and-forget spawns where you want to collect the result
later, without using `execute_on`.

---

## 4. Delegate Tasks (execute_on)

Use `ctx.execute_on()` to send a task to a child and wait for the result.

### Basic delegation

```python
async def handle_task(self, task, ctx) -> TaskResult:
    # Spawn a worker
    worker = await ctx.spawn(
        name="worker", role="task", cognitive_tier="operational",
        runtime_image="my_module:Worker", runtime_type="python",
    )

    # Delegate and wait for result
    result = await ctx.execute_on(
        pid=worker,
        description="Analyze this data",
        params={"input": "some data", "format": "json"},
        timeout_seconds=60,
    )

    # result is a TaskResult
    print(result.exit_code)   # 0
    print(result.output)      # "Analysis complete"
    print(result.artifacts)   # {"report": "..."}

    await ctx.kill(worker)
    return TaskResult(exit_code=0, output=result.output)
```

### ctx.execute_on() parameters

| Parameter | Type | Description |
|-----------|------|-------------|
| `pid` | int | Target child PID (must be your child) |
| `description` | str | Task description |
| `params` | dict[str, str] | Key-value parameters |
| `timeout_seconds` | int | 0 = no timeout |

**Returns**: `TaskResult` with `exit_code`, `output`, `artifacts`, `metadata`.

**Raises**: `RuntimeError` if the target is not your child (PPID ownership check).

### Sequential delegation

```python
# Process subtopics one at a time
for i, subtopic in enumerate(subtopics):
    result = await ctx.execute_on(
        pid=worker_pids[i],
        description=subtopic,
        params={"topic": subtopic},
    )
    findings.append(result.output)
```

### Parallel delegation

```python
import asyncio

# Delegate to all workers simultaneously
tasks = [
    ctx.execute_on(
        pid=worker_pids[i],
        description=subtopics[i],
        params={"topic": subtopics[i]},
    )
    for i in range(len(subtopics))
]
results = await asyncio.gather(*tasks)

for r in results:
    print(r.output)
```

**Note**: parallel delegation requires each worker to be a different PID.
A single worker can only handle one task at a time.

---

## 5. Share Artifacts

Artifacts are stored in the kernel's shared memory. Any agent can store
artifacts; visibility controls who can read them.

### Store an artifact

```python
artifact_id = await ctx.store_artifact(
    key="research-report",
    content=b"Report content here...",
    content_type="text/plain",
    visibility=3,  # global
)
```

### Visibility levels

| Level | Name | Who can read |
|-------|------|-------------|
| 0 | private | Only the owner process |
| 1 | user | Same user identity |
| 2 | subtree | Owner and descendants |
| 3 | global | Any process |

### Retrieve an artifact

```python
# By key
artifact = await ctx.get_artifact(key="research-report")
print(artifact.content)       # bytes
print(artifact.content_type)  # "text/plain"

# By ID
artifact = await ctx.get_artifact(artifact_id="abc-123")
```

### Store JSON data

```python
import json

report = {"topic": "AI", "findings": ["finding1", "finding2"]}
await ctx.store_artifact(
    key="report-final",
    content=json.dumps(report).encode("utf-8"),
    content_type="application/json",
)
```

### Artifact patterns

**Worker stores, lead reads**:
```python
# In worker's handle_task:
await ctx.store_artifact(
    key=f"result-{self.pid}",
    content=result_data.encode(),
    content_type="text/plain",
    visibility=3,
)

# In lead's handle_task (after execute_on returns):
artifact = await ctx.get_artifact(key=f"result-{worker_pid}")
```

**Using CoreClient (outside task context)**:
```python
# In on_init or on_shutdown:
artifacts = await self.core.list_artifacts(prefix="result-")
for art in artifacts:
    print(art.key, art.content_type, art.size_bytes)
```

---

## 6. Send Messages

Use `ctx.send()` to send IPC messages to other agents.

### Send to a child by PID

```python
msg_id = await ctx.send(
    to_pid=child_pid,
    type="instruction",
    payload=b"Please do X",
    priority="normal",
)
```

### Send to a named queue

```python
msg_id = await ctx.send(
    to_queue="notifications",
    type="alert",
    payload=b"System overloaded",
    priority="high",
)
```

### ctx.send() parameters

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `to_pid` | int | 0 | Target process PID |
| `to_queue` | str | "" | Named queue (alternative to PID) |
| `type` | str | "default" | Message type label |
| `payload` | bytes | b"" | Message content |
| `priority` | str | "normal" | "critical", "high", "normal", "low" |
| `requires_ack` | bool | False | Wait for acknowledgement |
| `ttl` | int | 0 | Time-to-live in seconds (0 = forever) |

### Handle incoming messages

Override `on_message()` in your agent:

```python
class MyAgent(HiveAgent):
    async def on_message(self, message) -> MessageAck:
        print(f"Got message from PID {message.from_pid}: {message.type}")
        print(f"Payload: {message.payload.decode()}")

        if message.type == "instruction":
            # Process instruction
            return MessageAck(status=MessageAck.ACK_ACCEPTED, reply="OK")
        else:
            return MessageAck(status=MessageAck.ACK_REJECTED, reply="Unknown type")
```

### Message routing rules

- **Parent <-> Child**: direct delivery
- **Siblings**: routed through broker, parent gets a copy
- **Cross-branch**: routed through nearest common ancestor (NCA)
- **Priority queue**: messages are ordered by effective priority with aging

### MessageAck statuses

| Status | Constant | Meaning |
|--------|----------|---------|
| 0 | `ACK_ACCEPTED` | Message processed |
| 1 | `ACK_QUEUED` | Queued for later |
| 2 | `ACK_REJECTED` | Refused |
| 3 | `ACK_ESCALATE` | Escalated to parent |

---

## 7. Handle Escalations

Escalations flow upward through the process tree. Use them to report
problems that the current agent cannot handle.

### Escalate a problem

```python
response = await ctx.escalate(
    issue="Worker PID 7 failed after 3 retries",
    severity="error",
    auto_propagate=True,
)
print(response)  # "acknowledged" or parent's response
```

### Severity levels

| Severity | Value | When to use |
|----------|-------|-------------|
| "info" | 0 | Informational, no action needed |
| "warning" | 1 | May need attention |
| "error" | 2 | Action required |
| "critical" | 3 | Immediate action required |

### auto_propagate

When `auto_propagate=True`, the escalation continues up the tree until
a handler responds. When `False`, only the immediate parent is notified.

### Escalation pattern

```python
class ResilientWorker(HiveAgent):
    async def handle_task(self, task, ctx) -> TaskResult:
        for attempt in range(3):
            try:
                result = do_work(task)
                return TaskResult(exit_code=0, output=result)
            except Exception as e:
                await ctx.log("warn", f"Attempt {attempt+1} failed: {e}")

        # All retries failed -- escalate to parent
        await ctx.escalate(
            issue=f"Task '{task.description}' failed after 3 attempts",
            severity="error",
        )
        return TaskResult(exit_code=1, output="Failed, escalated to parent")
```

---

## 8. Monitor Resources

### Report progress

Inside `handle_task()`, report progress so the caller can track status:

```python
await ctx.report_progress("Loading model...", 10.0)
# ... work ...
await ctx.report_progress("Processing data...", 50.0)
# ... work ...
await ctx.report_progress("Generating output...", 90.0)
```

Progress is sent through the Execute stream (not a syscall) and appears
in real-time on the caller side.

### Log messages

```python
await ctx.log("info", "Starting analysis")
await ctx.log("warn", "Missing optional field")
await ctx.log("error", "Cannot reach external API")
```

Log levels: `"debug"`, `"info"`, `"warn"`, `"error"`.

### Query resource usage (via CoreClient)

In `on_init()` or `on_shutdown()`, use `self.core` for direct gRPC calls:

```python
class ResourceAwareAgent(HiveAgent):
    async def on_init(self, config):
        usage = await self.core.get_resource_usage()
        print(f"Tokens remaining: {usage.tokens_remaining}")
        print(f"Children: {usage.children_active}/{usage.children_max}")
        print(f"Context: {usage.context_usage_percent}%")
        print(f"Uptime: {usage.uptime_seconds}s")
```

### Request additional resources

```python
result = await self.core.request_resources(
    resource_type="tokens",
    amount=10000,
)
if result["granted"]:
    print(f"Got {result['granted_amount']} tokens")
else:
    print(f"Denied: {result['reason']}")
```

---

## 9. Build a Team Scenario

This section walks through building a complete multi-agent team from scratch.

### Scenario: Data Analysis Team

A **DataLead** agent receives a dataset, spawns **Analyzer** workers,
delegates analysis subtasks, merges results, and produces a report.

### Step 1: Define the worker

```python
# agents/analyzer.py
import asyncio
from hivekernel_sdk import HiveAgent, TaskResult, AgentConfig

class Analyzer(HiveAgent):
    async def on_init(self, config):
        await self.core.log("info", f"Analyzer {self.pid} ready")

    async def handle_task(self, task, ctx) -> TaskResult:
        metric = task.params.get("metric", "unknown")
        dataset = task.params.get("dataset", "")

        await ctx.report_progress(f"Analyzing {metric}...", 30.0)
        await ctx.log("info", f"PID {self.pid} analyzing: {metric}")

        # Simulate analysis work
        await asyncio.sleep(0.5)

        finding = f"Analysis of '{metric}': mean=42.0, stddev=3.14"
        await ctx.report_progress("Done", 100.0)

        return TaskResult(
            exit_code=0,
            output=finding,
            metadata={"metric": metric, "worker_pid": str(self.pid)},
        )
```

### Step 2: Define the lead

```python
# agents/data_lead.py
import json
from hivekernel_sdk import HiveAgent, TaskResult, AgentConfig
from hivekernel_sdk.syscall import SyscallContext

class DataLead(HiveAgent):
    async def on_init(self, config):
        self.worker_count = int(config.metadata.get("worker_count", "3"))

    async def handle_task(self, task, ctx: SyscallContext) -> TaskResult:
        metrics = json.loads(task.params.get("metrics", "[]"))
        await ctx.log("info", f"DataLead starting: {len(metrics)} metrics")
        await ctx.report_progress("Spawning workers", 10.0)

        # 1. Spawn workers
        workers = []
        for i in range(len(metrics)):
            pid = await ctx.spawn(
                name=f"analyzer-{i}",
                role="task",
                cognitive_tier="operational",
                model="mini",
                runtime_image="agents.analyzer:Analyzer",
                runtime_type="python",
            )
            workers.append(pid)
            await ctx.log("info", f"Spawned analyzer-{i} as PID {pid}")

        await ctx.report_progress("Workers ready", 20.0)

        # 2. Delegate metrics to workers
        results = []
        for i, metric in enumerate(metrics):
            pct = 20.0 + (60.0 * (i + 1) / len(metrics))
            await ctx.report_progress(f"Analyzing: {metric}", pct)

            result = await ctx.execute_on(
                pid=workers[i],
                description=f"Analyze {metric}",
                params={"metric": metric, "dataset": task.params.get("dataset", "")},
            )
            results.append({
                "metric": metric,
                "finding": result.output,
                "worker_pid": workers[i],
            })

            # Store individual result
            await ctx.store_artifact(
                key=f"analysis-{metric.lower().replace(' ', '-')}",
                content=result.output.encode("utf-8"),
                content_type="text/plain",
            )

        await ctx.report_progress("All analysis complete", 85.0)

        # 3. Merge and store final report
        report = {
            "dataset": task.params.get("dataset", ""),
            "metrics_analyzed": len(metrics),
            "results": results,
        }
        report_json = json.dumps(report, indent=2)
        await ctx.store_artifact(
            key="analysis-report-final",
            content=report_json.encode("utf-8"),
            content_type="application/json",
        )

        # 4. Cleanup workers
        for pid in workers:
            await ctx.kill(pid)
        await ctx.log("info", "All workers terminated")

        return TaskResult(
            exit_code=0,
            output=f"Analyzed {len(metrics)} metrics with {len(workers)} workers",
            artifacts={"report": report_json},
            metadata={"metrics_count": str(len(metrics))},
        )
```

### Step 3: Run from an external script

```python
# run_analysis.py
import asyncio
import json
import os
import subprocess
import sys
import time

import grpc.aio
from hivekernel_sdk import agent_pb2, core_pb2, core_pb2_grpc

KERNEL_BIN = "bin/hivekernel.exe"
CORE_ADDR = "localhost:50051"
QUEEN_PID = 2

async def main():
    # 1. Start kernel
    sdk_dir = os.path.abspath("sdk/python")
    env = os.environ.copy()
    env["PYTHONPATH"] = sdk_dir

    kernel = subprocess.Popen(
        [KERNEL_BIN, "--listen", ":50051"],
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )

    # 2. Wait for readiness
    await asyncio.sleep(2)

    channel = grpc.aio.insecure_channel(CORE_ADDR)
    stub = core_pb2_grpc.CoreServiceStub(channel)
    queen_md = [("x-hivekernel-pid", str(QUEEN_PID))]
    king_md = [("x-hivekernel-pid", "1")]

    try:
        # 3. Spawn lead
        resp = await stub.SpawnChild(
            agent_pb2.SpawnRequest(
                name="data-lead",
                role=agent_pb2.ROLE_LEAD,
                cognitive_tier=agent_pb2.COG_TACTICAL,
                model="sonnet",
                runtime_type=agent_pb2.RUNTIME_PYTHON,
                runtime_image="agents.data_lead:DataLead",
            ),
            metadata=queen_md,
        )
        lead_pid = resp.child_pid
        print(f"Lead spawned: PID {lead_pid}")

        # 4. Execute analysis task
        metrics = ["Revenue", "User Growth", "Churn Rate"]
        exec_resp = await stub.ExecuteTask(
            core_pb2.ExecuteTaskRequest(
                target_pid=lead_pid,
                description="Analyze Q4 metrics",
                params={
                    "metrics": json.dumps(metrics),
                    "dataset": "q4-2026",
                },
                timeout_seconds=120,
            ),
            metadata=king_md,
            timeout=180,
        )

        print(f"Result: {exec_resp.result.output}")

        # 5. Cleanup
        await stub.KillChild(
            agent_pb2.KillRequest(target_pid=lead_pid, recursive=True),
            metadata=queen_md,
        )
    finally:
        await channel.close()
        kernel.terminate()
        kernel.wait(timeout=5)

asyncio.run(main())
```

### Key patterns in this example

1. **Lead-worker pattern**: lead spawns workers, delegates, merges, kills
2. **Cognitive tier enforcement**: lead is tactical, workers are operational
3. **Artifact chain**: workers store individual results, lead stores merged report
4. **Sequential delegation**: process one metric at a time (safe, predictable)
5. **Cleanup**: lead kills workers, external script kills lead

---

## 10. Call From External Systems

External systems interact with HiveKernel through the `CoreService` gRPC API.

### Connection setup

```python
import grpc.aio
from hivekernel_sdk import core_pb2, core_pb2_grpc, agent_pb2

CORE_ADDR = "localhost:50051"

async def connect():
    channel = grpc.aio.insecure_channel(CORE_ADDR)
    stub = core_pb2_grpc.CoreServiceStub(channel)
    return channel, stub
```

### Caller identity

Every gRPC call requires `x-hivekernel-pid` metadata to identify the caller:

```python
# Call as king (PID 1) -- full admin access
king_md = [("x-hivekernel-pid", "1")]

# Call as queen (PID 2)
queen_md = [("x-hivekernel-pid", "2")]

# Call as a specific agent
agent_md = [("x-hivekernel-pid", "5")]
```

### SpawnChild RPC

```python
resp = await stub.SpawnChild(
    agent_pb2.SpawnRequest(
        name="my-agent",
        role=agent_pb2.ROLE_WORKER,
        cognitive_tier=agent_pb2.COG_OPERATIONAL,
        model="mini",
        runtime_type=agent_pb2.RUNTIME_PYTHON,
        runtime_image="my_module:MyAgent",
    ),
    metadata=queen_md,
)
print(f"PID: {resp.child_pid}, success: {resp.success}")
```

### ExecuteTask RPC

```python
resp = await stub.ExecuteTask(
    core_pb2.ExecuteTaskRequest(
        target_pid=agent_pid,
        description="Do something",
        params={"key": "value"},
        timeout_seconds=60,
    ),
    metadata=king_md,
    timeout=120,  # gRPC deadline
)
if resp.success:
    print(f"Output: {resp.result.output}")
    print(f"Artifacts: {dict(resp.result.artifacts)}")
else:
    print(f"Error: {resp.error}")
```

### KillChild RPC

```python
resp = await stub.KillChild(
    agent_pb2.KillRequest(target_pid=agent_pid, recursive=True),
    metadata=queen_md,
)
print(f"Killed PIDs: {list(resp.killed_pids)}")
```

### GetProcessInfo RPC

```python
info = await stub.GetProcessInfo(
    core_pb2.ProcessInfoRequest(pid=agent_pid),
    metadata=king_md,
)
print(f"Name: {info.name}, State: {info.state}, Role: {info.role}")
```

### ListChildren RPC

```python
resp = await stub.ListChildren(
    core_pb2.ListChildrenRequest(recursive=True),
    metadata=king_md,
)
for child in resp.children:
    print(f"  PID {child.pid}: {child.name} (state={child.state})")
```

### ListArtifacts + GetArtifact RPCs

```python
# List all artifacts
arts = await stub.ListArtifacts(
    core_pb2.ListArtifactsRequest(prefix=""),
    metadata=queen_md,
)
for art in arts.artifacts:
    print(f"  {art.key}: {art.content_type} ({art.size_bytes}B)")

# Get specific artifact
art = await stub.GetArtifact(
    agent_pb2.GetArtifactRequest(key="my-artifact"),
    metadata=queen_md,
)
if art.found:
    print(art.content.decode("utf-8"))
```

### Full lifecycle pattern

The typical external integration follows this pattern:

```
SpawnChild -> ExecuteTask -> [read artifacts] -> KillChild
```

See `sdk/python/examples/demo_showcase.py` for a complete working example.

---

## 11. Run Kernel Programmatically

For scripts, tests, and demos, start the kernel as a subprocess.

### Basic pattern

```python
import os
import subprocess
import asyncio
import time

import grpc.aio
from hivekernel_sdk import core_pb2, core_pb2_grpc

KERNEL_BIN = os.path.abspath("bin/hivekernel.exe")
CORE_ADDR = "localhost:50051"

async def wait_for_kernel(addr, timeout=15.0):
    """Poll kernel gRPC until it responds."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            channel = grpc.aio.insecure_channel(addr)
            stub = core_pb2_grpc.CoreServiceStub(channel)
            await stub.GetProcessInfo(
                core_pb2.ProcessInfoRequest(pid=1),
                metadata=[("x-hivekernel-pid", "1")],
                timeout=2,
            )
            await channel.close()
            return
        except Exception:
            await asyncio.sleep(0.3)
    raise TimeoutError(f"Kernel not ready after {timeout}s")

def start_kernel():
    """Start kernel subprocess with proper environment."""
    env = os.environ.copy()
    sdk_dir = os.path.abspath("sdk/python")
    pythonpath = env.get("PYTHONPATH", "")
    if sdk_dir not in pythonpath:
        env["PYTHONPATH"] = sdk_dir + os.pathsep + pythonpath if pythonpath else sdk_dir

    proc = subprocess.Popen(
        [KERNEL_BIN, "--listen", ":50051"],
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    return proc

async def main():
    kernel = start_kernel()
    try:
        await wait_for_kernel(CORE_ADDR)
        print("Kernel ready!")
        # ... do your work ...
    finally:
        kernel.terminate()
        try:
            kernel.wait(timeout=5)
        except subprocess.TimeoutExpired:
            kernel.kill()
            kernel.wait()
        print("Kernel stopped")

asyncio.run(main())
```

### Critical details

1. **PYTHONPATH**: set to `sdk/python` so spawned agents can import `hivekernel_sdk`
2. **subprocess.DEVNULL**: use for both stdout and stderr to avoid pipe buffer
   deadlock on Windows
3. **Readiness probe**: poll `GetProcessInfo(pid=1)` until the kernel responds
4. **Cleanup in finally**: always terminate the kernel, even on error
5. **Port**: default `:50051`, pass `--listen :PORT` to change

---

## 12. LLM Integration

HiveKernel agents can make real LLM calls via OpenRouter. The SDK provides
`LLMClient` (low-level) and `LLMAgent` (high-level base class).

### Setup

1. Get an API key from [openrouter.ai](https://openrouter.ai/)
2. Create a `.env` file in the project root:
   ```
   OPENROUTER_API_KEY=sk-or-v1-your-key-here
   ```
3. The `.env` file is gitignored. See `.env.example` for the format.

### LLMClient

Low-level async client. Zero extra deps (uses `urllib.request` + `asyncio.to_thread`).

```python
from hivekernel_sdk import LLMClient

client = LLMClient(api_key="sk-or-v1-...", default_model="anthropic/claude-sonnet-4-5")

# Single prompt
response = await client.complete("What is 2+2?")

# Multi-turn chat
response = await client.chat([
    {"role": "user", "content": "Hello"},
    {"role": "assistant", "content": "Hi! How can I help?"},
    {"role": "user", "content": "What is Python?"},
], system="You are a helpful assistant.")

# Token tracking
print(f"Total tokens used: {client.total_tokens}")
```

### Model map

The `model` parameter accepts either a short alias or a full OpenRouter model ID:

| Alias | OpenRouter Model ID |
|-------|---------------------|
| `"opus"` | `anthropic/claude-opus-4` |
| `"sonnet"` | `anthropic/claude-sonnet-4-5` |
| `"haiku"` | `anthropic/claude-haiku-3-5` |
| `"mini"` | `google/gemini-2.5-flash` |

These aliases match HiveKernel's tier-to-model mapping (strategic=opus,
tactical=sonnet, operational=mini).

### LLMAgent

High-level base class that extends `HiveAgent`. Auto-creates an `LLMClient`
from `OPENROUTER_API_KEY` env var and the agent's `model` config field.

```python
from hivekernel_sdk import LLMAgent, TaskResult

class SmartWorker(LLMAgent):
    async def handle_task(self, task, ctx) -> TaskResult:
        # self.ask() uses the agent's model and system_prompt
        answer = await self.ask(f"Analyze: {task.description}")

        # self.chat() for multi-turn
        followup = await self.chat([
            {"role": "user", "content": "Summarize the analysis"},
            {"role": "assistant", "content": answer},
            {"role": "user", "content": "Now give 3 key takeaways"},
        ])

        return TaskResult(exit_code=0, output=followup)
```

When spawning an `LLMAgent`, set `model` and `system_prompt` in the spawn request:

```python
pid = await ctx.spawn(
    name="smart-worker",
    role="task",
    cognitive_tier="operational",
    model="mini",                    # -> google/gemini-2.5-flash
    system_prompt="You are a data analyst.",
    runtime_image="my_module:SmartWorker",
    runtime_type="python",
)
```

### LLMAgent vs HiveAgent

| Feature | HiveAgent | LLMAgent |
|---------|-----------|----------|
| Base class | - | HiveAgent |
| LLM client | manual | automatic (`self.llm`) |
| `self.ask()` | no | yes |
| `self.chat()` | no | yes |
| Requires API key | no | yes (OPENROUTER_API_KEY) |
| Use when | no LLM needed | agent needs LLM calls |

### Run the LLM demo

```bash
# With web dashboard (auto-opens browser, pauses at each step)
python sdk/python/examples/llm_research_team.py

# Without dashboard (runs straight through)
python sdk/python/examples/llm_research_team.py --no-dashboard
```

The demo starts the kernel, launches the web dashboard, and opens
http://localhost:8080 in your browser. You'll see:

1. Initial tree (king + queen + demo-worker)
2. **Press Enter** -- lead agent appears on the tree
3. **Press Enter** -- LLM research starts, workers appear and work in real-time
4. **Press Enter** -- zombie workers visible (gray, 40% opacity)
5. **Press Enter** -- cleanup, everything shuts down

All calls are real LLM requests through OpenRouter (~2000 tokens total).

---

## 13. Troubleshooting

### READY timeout (agent spawn hangs)

**Symptom**: `SpawnChild` returns error "process not ready" or hangs.

**Cause**: the Python agent process did not print `READY <port>` in time.

**Fix**:
- Check that the `runtime_image` class exists and is importable
- Check `PYTHONPATH` includes `sdk/python`
- Check Python is on `PATH`
- Check the module has no import errors: `python -c "from my_module import MyClass"`

### Unicode error on Windows

**Symptom**: `UnicodeEncodeError: 'charmap' codec can't encode character`

**Cause**: Windows console uses cp1251/cp1252, Python defaults to it for stdout.

**Fix**: use ASCII-only characters in all `print()` statements. Avoid arrows,
emojis, and box-drawing characters.

### Pipe buffer deadlock

**Symptom**: kernel subprocess hangs when spawning agents.

**Cause**: kernel writes to stdout/stderr faster than the pipe buffer is read.

**Fix**: always use `subprocess.DEVNULL` for both stdout and stderr:
```python
subprocess.Popen(
    [KERNEL_BIN, "--listen", ":50051"],
    stdout=subprocess.DEVNULL,
    stderr=subprocess.DEVNULL,
)
```

### Cognitive tier denied

**Symptom**: `spawn failed: cognitive tier of child must be <= parent`

**Cause**: tried to spawn a child with a higher tier than the parent.

**Fix**: check tier hierarchy: strategic > tactical > operational.
A tactical parent cannot spawn a strategic child.

### PPID ownership error

**Symptom**: `execute_on failed: caller PID X is not parent of target PID Y`

**Cause**: you can only `execute_on` your own children.

**Fix**: verify the target PID is a direct child of the calling agent.
Use `self.core.list_children()` to check your children.

### Proto import error

**Symptom**: `ImportError: cannot import name 'agent_pb2'`

**Cause**: after regenerating proto files, the generated Python files use
absolute imports instead of relative.

**Fix**: in `sdk/python/hivekernel_sdk/`, edit these 3 files:
- `agent_pb2_grpc.py`
- `core_pb2.py`
- `core_pb2_grpc.py`

Change `import agent_pb2` to `from . import agent_pb2` (and similar for core_pb2).

### Task role cannot be strategic

**Symptom**: spawn fails with role/tier validation error.

**Fix**: agents with `role="task"` cannot have `cognitive_tier="strategic"`.
Use "tactical" or "operational" for task-role agents.

### Agent not found after spawn

**Symptom**: `execute_on` or `kill` fails with "process not found".

**Cause**: the agent process may have crashed immediately after spawn.

**Fix**:
- Add a brief `await asyncio.sleep(0.5)` after spawn to let the agent initialize
- Check agent logs for import errors or exceptions in `on_init()`
- Verify the `runtime_image` format is `"module:ClassName"` (colon separator)

### Connection refused

**Symptom**: `grpc._channel._InactiveRpcError: failed to connect to all addresses`

**Cause**: kernel is not running or listening on a different port.

**Fix**:
- Verify kernel is running: `tasklist | findstr hivekernel`
- Check the port matches: default is `:50051`
- Use the readiness probe pattern from section 11

---

## Quick Reference

### SyscallContext methods (inside handle_task)

| Method | Returns | Description |
|--------|---------|-------------|
| `ctx.spawn(...)` | `int` (PID) | Spawn a child agent |
| `ctx.kill(pid)` | `list[int]` | Kill agent and subtree |
| `ctx.send(...)` | `str` (msg_id) | Send IPC message |
| `ctx.execute_on(pid, ...)` | `TaskResult` | Delegate task to child |
| `ctx.wait_child(pid, timeout)` | `dict` | Wait for child exit (like waitpid) |
| `ctx.store_artifact(...)` | `str` (art_id) | Store artifact in shared memory |
| `ctx.get_artifact(...)` | artifact proto | Retrieve artifact |
| `ctx.escalate(...)` | `str` | Escalate issue to parent |
| `ctx.log(level, msg)` | None | Write log entry |
| `ctx.report_progress(msg, pct)` | None | Send progress update |

### CoreClient methods (via self.core)

| Method | Returns | Description |
|--------|---------|-------------|
| `core.spawn_child(...)` | `int` (PID) | Spawn child (RPC) |
| `core.kill_child(pid)` | `list[int]` | Kill child (RPC) |
| `core.get_process_info(pid)` | proto | Process info |
| `core.list_children(recursive)` | list | List children |
| `core.send_message(...)` | `str` (msg_id) | Send message (RPC) |
| `core.store_artifact(...)` | `str` (art_id) | Store artifact (RPC) |
| `core.get_artifact(...)` | proto | Get artifact (RPC) |
| `core.list_artifacts(prefix)` | list | List artifacts |
| `core.get_resource_usage()` | `ResourceUsage` | Resource stats |
| `core.request_resources(...)` | dict | Request tokens |
| `core.escalate(...)` | `str` | Escalate issue |
| `core.log(level, msg)` | None | Write log |
| `core.report_metric(...)` | None | Report metric |

### CoreService RPCs (via gRPC stub)

| RPC | Request | Description |
|-----|---------|-------------|
| `SpawnChild` | `SpawnRequest` | Spawn a child process |
| `KillChild` | `KillRequest` | Kill process + subtree |
| `ExecuteTask` | `ExecuteTaskRequest` | Send task and wait for result |
| `GetProcessInfo` | `ProcessInfoRequest` | Query process state |
| `ListChildren` | `ListChildrenRequest` | List child processes |
| `SendMessage` | `SendMessageRequest` | Send IPC message |
| `StoreArtifact` | `StoreArtifactRequest` | Store shared artifact |
| `GetArtifact` | `GetArtifactRequest` | Retrieve artifact |
| `ListArtifacts` | `ListArtifactsRequest` | List artifacts |
| `GetResourceUsage` | `ResourceUsageRequest` | Get resource stats |
| `RequestResources` | `ResourceRequest` | Request more tokens |
| `Escalate` | `EscalateRequest` | Escalate to parent |
| `Log` | `LogRequest` | Write log entry |
| `ReportMetric` | `MetricRequest` | Report metric |
| `SetVPSConfig` | `VPSConfigRequest` | Configure VPS |
| `GetVPSConfig` | `VPSConfigQuery` | Query VPS config |
| `MigrateProcess` | `MigrateRequest` | Migrate process branch |
