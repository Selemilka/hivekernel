# Plan 003: Practical Agents — Chat, GitHub Monitor, Coder

**Based on:** OpenClaw migration, user requirements
**Status:** TODO
**Depends on:** Plan 002 (done), Event Sourcing (done)

---

## Overview

Three real-world agents that turn HiveKernel from a demo into a usable tool:

1. **Assistant** — 24/7 chat interface in the dashboard, answers questions, schedules cron tasks
2. **GitHub Monitor** — cron-triggered daemon that checks repos for updates
3. **Coder** — on-demand coding agent, invoked by other agents via `execute_on`

Plus the infrastructure to support them: cron engine activation and chat web UI.

## Process Tree

```
King (PID 1)
  └── Queen (PID 2, daemon, tactical)
        ├── Maid (daemon, operational)          — health checks
        ├── Assistant (daemon, tactical, sonnet) — 24/7 chat, cron management
        ├── GitMonitor (daemon, operational, mini) — cron-triggered repo checks
        └── Coder (daemon, tactical, sonnet)     — on-demand code generation
```

All three new agents are **daemons** (stay alive, auto-restart on crash).
Queen spawns them in `on_init()` alongside Maid.

## Execution Order

```
Phase 1: Cron Engine Activation (Go, ~120 LOC)
    |
Phase 2: Assistant Agent + Chat UI (Python + HTML/JS, ~600 LOC)
    |
Phase 3: GitHub Monitor (Python, ~200 LOC)
    |
Phase 4: Coder Agent (Python, ~150 LOC)
    |
Phase 5: Integration & E2E Demo (~200 LOC)
```

---

### Phase 1: Cron Engine Activation

**Goal:** The Go kernel's CronScheduler already parses cron expressions and tracks
entries, but nothing actually polls `CheckDue()`. Wire it up so cron entries fire
on schedule.

**Already implemented (internal/scheduler/cron.go, 271 lines, 10 tests):**
- `CronScheduler` — full CRUD: `Add`, `Remove`, `Get`, `SetEnabled`, `List`, `ListByVPS`
- `ParseCron(expr)` — parses `"*/5 * * * *"` style expressions (minute/hour/dom/month/dow)
- `CronSchedule.Matches(time)` — checks if a time matches the schedule
- `CheckDue(now)` — returns due entries, deduplicates within same minute via `LastRun`
- `CronEntry` struct with `TargetPID`, `SpawnName/Role/Tier/Parent`, `KeepAlive`, `VPS`
- Two action types: `CronSpawn` (spawn new process), `CronWake` (wake sleeping process)
- King creates `CronScheduler` in `New()`, exposes via `King.Cron()` accessor
- Proto: `Schedule { cron, keep_alive }` field exists in `SpawnRequest` (parsed but not wired)

**What's missing:**
- No goroutine calls `CheckDue()` — the scheduler ticks silently into the void
- No `CronExecute` action — can't tell an existing daemon to run a task
- No gRPC API to create/list/remove entries from agents or external clients
- No Python CoreClient methods for cron management

**Solution:**

#### 1a. Cron Poller Goroutine

In `King`, start a background goroutine that ticks every 30 seconds:

```go
// In King.Start() or wired from main.go:
func (k *King) runCronPoller(ctx context.Context) {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            due := k.cron.CheckDue(time.Now())
            for _, entry := range due {
                k.executeCronEntry(entry)
            }
        }
    }
}
```

30-second tick is good enough (cron has minute granularity, `CheckDue` deduplicates
via `LastRun`).

#### 1b. CronExecute Action Type

Existing actions: `CronSpawn` (spawn new process), `CronWake` (wake sleeping process).
Neither fits our use case — we need to send a task to an **already running** daemon.

`CronSpawn` would mean spawning a new GitMonitor every 30 minutes (wasteful, loses state).
`CronWake` changes state from Sleeping→Running but doesn't send a task.

**New action `CronExecute`**: calls `Executor.ExecuteTask(targetPID, desc, params)` on
an existing daemon. This is the primary use case for Plan 003 agents.

The infrastructure for executing tasks on agents already exists:
- `internal/runtime/executor.go` — `ExecuteTask()` opens bidi Execute stream to agent
- `internal/kernel/grpc_core.go` — `ExecuteTask` RPC handler
- Works today via gRPC and dashboard — just needs wiring from cron poller

Add third action type to cron.go:

```go
const (
    CronSpawn   CronAction = iota
    CronWake
    CronExecute  // NEW: execute task on existing process
)

type CronEntry struct {
    // ... existing fields ...
    ExecuteDesc   string            // task description for CronExecute
    ExecuteParams map[string]string // task params for CronExecute
}
```

#### 1c. gRPC API for Cron Management

New RPCs on CoreService:

```protobuf
message CronEntry {
    string id = 1;
    string name = 2;
    string cron_expression = 3;
    string action = 4;              // "execute"
    uint32 target_pid = 5;          // target for execute
    string execute_description = 6;
    map<string, string> execute_params = 7;
    bool enabled = 8;
}

message AddCronRequest {
    string name = 1;
    string cron_expression = 2;
    uint32 target_pid = 3;
    string execute_description = 4;
    map<string, string> execute_params = 5;
}
message AddCronResponse { string cron_id = 1; }

message RemoveCronRequest { string cron_id = 1; }
message RemoveCronResponse { bool ok = 1; }

message ListCronRequest {}
message ListCronResponse { repeated CronEntry entries = 1; }

// In CoreService:
rpc AddCron(AddCronRequest) returns (AddCronResponse);
rpc RemoveCron(RemoveCronRequest) returns (RemoveCronResponse);
rpc ListCron(ListCronRequest) returns (ListCronResponse);
```

#### 1d. Python CoreClient Methods

```python
async def add_cron(self, name, cron_expression, target_pid, description, params=None) -> str:
    """Add a cron entry. Returns cron_id."""

async def remove_cron(self, cron_id) -> bool:
    """Remove a cron entry."""

async def list_cron(self) -> list[dict]:
    """List all cron entries."""
```

**Files:**
- `internal/scheduler/cron.go` — add `CronExecute` action type, `ExecuteDesc/Params` fields
- `internal/kernel/king.go` — `runCronPoller()`, `executeCronEntry()`
- `cmd/hivekernel/main.go` — start cron poller goroutine
- `api/proto/core.proto` — AddCron/RemoveCron/ListCron RPCs + messages
- `api/proto/hivepb/` — regen Go proto
- `internal/kernel/grpc_core.go` — implement 3 cron RPCs
- `sdk/python/hivekernel_sdk/core_pb2*.py` — regen Python proto (fix imports)
- `sdk/python/hivekernel_sdk/client.py` — add_cron, remove_cron, list_cron methods
- `internal/scheduler/cron_test.go` — tests for CronExecute action
- `internal/kernel/king_test.go` — test cron poller integration (optional)

**Risk:** low. CronScheduler is tested, just needs wiring.

---

### Phase 2: Assistant Agent + Chat UI

**Goal:** A chat interface in the dashboard where the user talks to an LLM-powered
assistant that can answer questions, schedule cron tasks, and delegate to other agents.

#### 2a. AssistantAgent (Python)

New file: `sdk/python/hivekernel_sdk/assistant.py`

```python
class AssistantAgent(LLMAgent):
    """24/7 chat assistant daemon.

    Handles user messages via execute_on. Can:
    - Answer questions using LLM
    - Schedule cron tasks (via CoreClient.add_cron)
    - Delegate to Coder agent (via ctx.execute_on)
    - Report GitHub updates (receives from GitMonitor via send)
    """

    async def handle_task(self, task, ctx) -> TaskResult:
        message = task.params.get("message", task.description)
        history = json.loads(task.params.get("history", "[]"))

        # Build context from IPC inbox (any pending messages from other agents)
        notifications = await self._check_notifications()

        # System prompt with capabilities
        system = (
            "You are HiveKernel Assistant, a helpful AI running inside a "
            "process tree OS. You can:\n"
            "- Answer questions\n"
            "- Schedule recurring tasks (cron): say 'I'll schedule that'\n"
            "- Ask the Coder agent to write code\n"
            "- Check GitHub repo status\n"
            "Respond concisely. If the user asks to schedule something, "
            "extract: what, how often (cron expression), which agent."
        )

        messages = [{"role": "system", "content": system}]
        # Add chat history (last N messages for context)
        for h in history[-10:]:
            messages.append(h)
        if notifications:
            messages.append({
                "role": "system",
                "content": f"Recent notifications:\n{notifications}"
            })
        messages.append({"role": "user", "content": message})

        response = await self.chat(messages, max_tokens=1024)

        # Check if response contains scheduling intent
        # (Could use structured output, but simple keyword check for v1)
        if self._has_schedule_intent(response):
            cron_info = await self._extract_cron_info(message, response)
            if cron_info:
                await self._create_cron_entry(cron_info)
                response += f"\n\n[Scheduled: {cron_info['name']} — {cron_info['cron']}]"

        return TaskResult(exit_code=0, output=response)

    async def _create_cron_entry(self, info):
        """Create cron entry via CoreClient."""
        await self.core.add_cron(
            name=info["name"],
            cron_expression=info["cron"],
            target_pid=info["target_pid"],
            description=info["description"],
            params=info.get("params"),
        )
```

Key design decisions:
- Daemon role, stays alive between chat messages
- Each chat message = one `execute_on` call
- Chat history passed as JSON in params (web UI manages history)
- Notifications from other agents read from IPC inbox
- Cron scheduling: LLM detects intent → extracts cron expression → creates entry

#### 2b. Chat Web Page

New files in `sdk/python/dashboard/static/`:
- `chat.html` — chat page layout
- `chat.js` — chat logic (send/receive, history, WS for live updates)

Layout:
```
┌──────────────────────────────────────────────────┐
│ HEADER: HiveKernel | [Tree] [Chat] | Status      │
├──────────────────────────────────────────────────┤
│                                                   │
│  ┌─────────────────────────────────────────────┐  │
│  │          Chat Messages                      │  │
│  │  ┌──────────────────────┐                   │  │
│  │  │ User: Hello          │                   │  │
│  │  └──────────────────────┘                   │  │
│  │                   ┌──────────────────────┐  │  │
│  │                   │ Assistant: Hi! I'm...│  │  │
│  │                   └──────────────────────┘  │  │
│  │                                             │  │
│  └─────────────────────────────────────────────┘  │
│                                                   │
│  ┌─────────────────────────────────────────────┐  │
│  │ [Type a message...                   ] [Send] │ │
│  └─────────────────────────────────────────────┘  │
│                                                   │
│  Sidebar (collapsible):                           │
│  - Scheduled tasks (cron list)                    │
│  - Active agents (process tree mini-view)         │
│  - Recent notifications                           │
└──────────────────────────────────────────────────┘
```

Features:
- Dark theme matching dashboard
- Chat history in localStorage (persists across refreshes)
- Typing indicator while waiting for response
- Sidebar: cron entries list, mini process tree, notifications
- Navigation: header links to Tree (/) and Chat (/chat.html)

#### 2c. Backend Endpoint

In `sdk/python/dashboard/app.py`:

```python
@app.post("/api/chat")
async def api_chat(body: dict):
    """Send message to Assistant agent, return response."""
    message = body.get("message", "")
    history = body.get("history", [])

    # Find Assistant PID in tree_cache
    assistant_pid = None
    for pid, node in tree_cache.items():
        if node.get("name", "").startswith("assistant"):
            assistant_pid = pid
            break

    if not assistant_pid:
        return {"ok": False, "error": "Assistant agent not found"}

    # Execute task on assistant
    result = await stub.ExecuteTask(
        core_pb2.ExecuteTaskRequest(
            target_pid=assistant_pid,
            description=message,
            params={"message": message, "history": json.dumps(history)},
        ),
        metadata=[("x-hivekernel-pid", str(queen_pid))],
    )
    return {
        "ok": True,
        "response": result.output,
        "exit_code": result.exit_code,
    }

@app.get("/api/cron")
async def api_cron_list():
    """List all cron entries."""
    resp = await stub.ListCron(core_pb2.ListCronRequest(), metadata=md(1))
    return {"entries": [entry_to_dict(e) for e in resp.entries]}

@app.post("/api/cron")
async def api_cron_add(body: dict):
    """Add a cron entry."""
    resp = await stub.AddCron(core_pb2.AddCronRequest(...), metadata=md(1))
    return {"cron_id": resp.cron_id}

@app.delete("/api/cron/{cron_id}")
async def api_cron_remove(cron_id: str):
    """Remove a cron entry."""
    resp = await stub.RemoveCron(
        core_pb2.RemoveCronRequest(cron_id=cron_id), metadata=md(1)
    )
    return {"ok": resp.ok}
```

#### 2d. Queen: Spawn Assistant

In `queen.py` `on_init()`, spawn Assistant alongside Maid:

```python
async def _spawn_assistant(self):
    self._assistant_pid = await self.core.spawn_child(
        name="assistant",
        role="daemon",
        cognitive_tier="tactical",
        runtime_image="hivekernel_sdk.assistant:AssistantAgent",
        runtime_type="python",
    )
```

**Files:**
- `sdk/python/hivekernel_sdk/assistant.py` — NEW (~200 lines)
- `sdk/python/hivekernel_sdk/__init__.py` — export AssistantAgent
- `sdk/python/hivekernel_sdk/queen.py` — spawn assistant in on_init
- `sdk/python/dashboard/static/chat.html` — NEW (~150 lines)
- `sdk/python/dashboard/static/chat.js` — NEW (~200 lines)
- `sdk/python/dashboard/static/style.css` — add chat styles (~50 lines)
- `sdk/python/dashboard/app.py` — add /api/chat, /api/cron endpoints
- `sdk/python/dashboard/static/index.html` — add nav link to chat
- `sdk/python/tests/test_assistant.py` — NEW (~100 lines)

**Risk:** medium. Chat UX needs iteration, LLM intent detection is fuzzy.

---

### Phase 3: GitHub Monitor

**Goal:** A daemon that checks GitHub repos for new commits on a cron schedule.
Reports updates to the Assistant (which can notify the user in chat).

#### 3a. GitHubMonitorAgent

New file: `sdk/python/hivekernel_sdk/github_monitor.py`

```python
class GitHubMonitorAgent(LLMAgent):
    """Monitors GitHub repos for updates.

    Triggered by cron via execute_on. Each run:
    1. Fetch latest commits from configured repos
    2. Compare with stored state (artifact)
    3. If new commits: store update, notify parent via escalate
    """

    async def handle_task(self, task, ctx) -> TaskResult:
        repos = json.loads(task.params.get("repos", "[]"))
        if not repos:
            repos = [task.params.get("repo", "Selemilka/hivekernel")]

        all_updates = []
        for repo in repos:
            updates = await self._check_repo(repo, ctx)
            all_updates.extend(updates)

        if all_updates:
            summary = self._format_updates(all_updates)
            await ctx.escalate(
                severity="info",
                issue=f"GitHub updates: {len(all_updates)} new commits",
            )
            await ctx.store_artifact(
                key="github/latest-updates",
                content=json.dumps(all_updates, ensure_ascii=False).encode(),
                content_type="application/json",
            )
            return TaskResult(exit_code=0, output=summary)

        return TaskResult(exit_code=0, output="No new updates")

    async def _check_repo(self, repo, ctx):
        """Fetch latest commits, compare with stored state."""
        url = f"https://api.github.com/repos/{repo}/commits?per_page=5"
        commits = await self._http_get_json(url)

        # Load previous state
        try:
            artifact = await self.core.get_artifact(f"github/state/{repo}")
            known_shas = json.loads(artifact.content.decode())
        except Exception:
            known_shas = []

        new_commits = [c for c in commits if c["sha"] not in known_shas]

        # Store new state
        current_shas = [c["sha"] for c in commits]
        await ctx.store_artifact(
            key=f"github/state/{repo}",
            content=json.dumps(current_shas).encode(),
        )

        return [
            {"repo": repo, "sha": c["sha"][:7],
             "message": c["commit"]["message"].split("\n")[0],
             "author": c["commit"]["author"]["name"],
             "date": c["commit"]["author"]["date"]}
            for c in new_commits
        ]

    async def _http_get_json(self, url):
        """Simple HTTP GET using urllib (no deps)."""
        import urllib.request
        import asyncio
        req = urllib.request.Request(url, headers={
            "Accept": "application/vnd.github.v3+json",
            "User-Agent": "HiveKernel-GitMonitor/1.0",
        })
        def _fetch():
            with urllib.request.urlopen(req, timeout=15) as resp:
                return json.loads(resp.read().decode())
        return await asyncio.to_thread(_fetch)
```

Simplest approach: GitHub public API, no auth needed for public repos.
Rate limit: 60 req/hour unauthenticated (plenty for monitoring).

#### 3b. Queen: Spawn GitMonitor + Default Cron

Queen spawns GitMonitor and creates a default cron entry:

```python
async def _spawn_github_monitor(self):
    self._gitmonitor_pid = await self.core.spawn_child(
        name="github-monitor",
        role="daemon",
        cognitive_tier="operational",
        runtime_image="hivekernel_sdk.github_monitor:GitHubMonitorAgent",
        runtime_type="python",
    )
    # Schedule default check every 30 minutes
    await self.core.add_cron(
        name="github-check",
        cron_expression="*/30 * * * *",
        target_pid=self._gitmonitor_pid,
        description="Check GitHub repos for updates",
        params={"repos": json.dumps(["Selemilka/hivekernel"])},
    )
```

**Files:**
- `sdk/python/hivekernel_sdk/github_monitor.py` — NEW (~150 lines)
- `sdk/python/hivekernel_sdk/__init__.py` — export GitHubMonitorAgent
- `sdk/python/hivekernel_sdk/queen.py` — spawn github-monitor + cron entry
- `sdk/python/tests/test_github_monitor.py` — NEW (~80 lines)

**Risk:** low. Simple HTTP fetch + artifact compare. No auth needed for public repos.

---

### Phase 4: Coder Agent

**Goal:** An LLM-powered daemon that accepts coding tasks via `execute_on`.
Other agents (Assistant, Lead, Worker) can delegate "write code" requests to it.

#### 4a. CoderAgent

New file: `sdk/python/hivekernel_sdk/coder.py`

```python
class CoderAgent(LLMAgent):
    """On-demand code generation daemon.

    Accepts tasks via execute_on. Returns structured code output.
    Uses a powerful model (sonnet) for quality code generation.
    """

    async def handle_task(self, task, ctx) -> TaskResult:
        request = task.params.get("request", task.description)
        language = task.params.get("language", "python")
        context = task.params.get("context", "")

        system = (
            f"You are a skilled {language} programmer. Write clean, "
            "well-structured code. Return ONLY the code block with brief "
            "comments. No explanations outside the code unless asked."
        )

        prompt = f"Task: {request}"
        if context:
            prompt = f"Context:\n{context}\n\n{prompt}"

        response = await self.ask(prompt, system_prompt=system, max_tokens=2048)

        # Store code as artifact
        safe_key = request.lower().replace(" ", "-")[:40]
        await ctx.store_artifact(
            key=f"code/{safe_key}",
            content=response.encode("utf-8"),
            content_type=f"text/{language}",
        )

        return TaskResult(exit_code=0, output=response)
```

Key design:
- Daemon role (stays alive, reusable)
- Powerful model (sonnet by default)
- Stores generated code as artifact
- Simple interface: pass `request` + optional `language` and `context`

#### 4b. Integration: Assistant Delegates to Coder

The Assistant detects coding requests and delegates:

```python
# In AssistantAgent.handle_task():
if self._is_coding_request(message):
    coder_pid = await self._find_agent("coder")
    if coder_pid:
        result = await ctx.execute_on(
            pid=coder_pid,
            description=message,
            params={"request": message, "language": detected_lang},
        )
        return TaskResult(exit_code=0, output=result.output)
```

#### 4c. Queen: Spawn Coder

```python
async def _spawn_coder(self):
    self._coder_pid = await self.core.spawn_child(
        name="coder",
        role="daemon",
        cognitive_tier="tactical",
        runtime_image="hivekernel_sdk.coder:CoderAgent",
        runtime_type="python",
    )
```

**Files:**
- `sdk/python/hivekernel_sdk/coder.py` — NEW (~100 lines)
- `sdk/python/hivekernel_sdk/__init__.py` — export CoderAgent
- `sdk/python/hivekernel_sdk/queen.py` — spawn coder
- `sdk/python/hivekernel_sdk/assistant.py` — delegate coding tasks to coder
- `sdk/python/tests/test_coder.py` — NEW (~60 lines)

**Risk:** low. Simple LLM wrapper with artifact storage.

---

### Phase 5: Integration & E2E Demo

**Goal:** Everything works together. User can chat, schedule GitHub checks,
request code generation — all through the chat interface.

#### 5a. E2E Test Script

`sdk/python/examples/test_plan003_e2e.py`:

```
1. Start kernel
2. Wait for Queen + Maid + Assistant + GitMonitor + Coder to spawn
3. Chat with Assistant: "Hello, what can you do?"
4. Ask: "Check the hivekernel repo on GitHub"
   → Assistant delegates to GitMonitor (or coder?)
5. Ask: "Schedule GitHub checks every 30 minutes"
   → Assistant creates cron entry
6. Verify cron entry exists (ListCron)
7. Ask: "Write a Python function that calculates Fibonacci"
   → Assistant delegates to Coder
   → Coder returns code
8. Verify artifacts stored
9. Print full process tree
```

#### 5b. Dashboard Enhancements

- Add navigation header to both pages (Tree / Chat)
- Show cron entries in chat sidebar
- Show notifications from GitMonitor in chat

**Files:**
- `sdk/python/examples/test_plan003_e2e.py` — NEW (~300 lines)
- `sdk/python/dashboard/static/index.html` — add nav header
- QUICKSTART.md — update with new features
- CHANGELOG.md — document Plan 003

**Risk:** low. Integration of existing pieces.

---

## Data Flow Diagrams

### Chat Flow
```
User (browser)
  │  POST /api/chat {message, history}
  ▼
Dashboard (FastAPI)
  │  gRPC ExecuteTask(assistant_pid, message)
  ▼
Assistant Agent (Python daemon)
  │  LLM call via OpenRouter
  │  (optional) execute_on(coder_pid, ...) for coding tasks
  │  (optional) core.add_cron(...) for scheduling
  ▼
Response → FastAPI → JSON → Browser
```

### GitHub Monitor Flow
```
Cron Poller (Go, every 30s)
  │  CheckDue() → GitMonitor entry is due
  │  ExecuteTask(gitmonitor_pid, "check repos")
  ▼
GitMonitor Agent (Python daemon)
  │  HTTP GET github.com/api/repos/.../commits
  │  Compare with stored artifact
  │  If new: store_artifact + escalate to parent
  ▼
Escalation → Queen → (optionally) notify Assistant
```

### Coder Flow
```
Assistant (or any agent)
  │  execute_on(coder_pid, "write fibonacci function")
  ▼
Coder Agent (Python daemon)
  │  LLM call (sonnet model)
  │  store_artifact("code/fibonacci", code)
  ▼
Code output → back to caller
```

---

## Implementation Order

1. Phase 1: Cron engine (Go) — foundation for scheduled agents
2. Phase 2: Assistant + Chat UI — primary user interface
3. Phase 3: GitHub Monitor — first scheduled agent
4. Phase 4: Coder — first delegated specialist
5. Phase 5: Integration test, docs

Each phase is independently testable and committable.

---

## Verification

**Phase 1:** `go test ./internal/... -v` passes. Start kernel → add cron via
gRPC client → verify it fires on schedule (check logs).

**Phase 2:** Start kernel + dashboard → open /chat.html → type message →
get LLM response. Schedule a cron task via chat → verify entry created.

**Phase 3:** Trigger GitMonitor manually → verify it fetches commits and
stores artifact. Set up cron → verify periodic execution.

**Phase 4:** Execute coding task on Coder → verify code returned and stored
as artifact.

**Phase 5:** Full E2E: chat → schedule → code → all artifacts present.
Dashboard shows full tree: King → Queen → Maid + Assistant + GitMonitor + Coder.

---

## Open Questions

1. **Auth for GitHub?** v1 uses unauthenticated API (60 req/hour). Good enough
   for a few repos. Can add token later via `.env`.

2. **Chat history persistence?** v1: localStorage in browser. v2: store as
   artifacts in kernel.

3. **Assistant cron detection?** v1: keyword heuristic + LLM fallback.
   v2: structured tool-use output from LLM.

4. **Multiple repos?** v1: configured in Queen's spawn params. v2: user
   configures via chat ("watch repo X").
