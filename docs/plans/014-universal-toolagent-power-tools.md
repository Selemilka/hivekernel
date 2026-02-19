# Plan 014: Universal ToolAgent + Power Tools

**Status:** COMPLETE (2026-02-19)

## Context

Currently HiveKernel has 8 specialized Python agent classes (Queen, Assistant,
Coder, Worker, Orchestrator, Architect, Maid, GitHubMonitor), each with
hardcoded behavior. Most of this behavior (60-100%) can be replaced by
configuring a single universal `ToolAgent` with:

- **System prompt from .md file** -- defines behavior, personality, instructions
- **Tool set from config** -- defines capabilities (which tools the agent gets)
- **Kernel constraints** -- role, cognitive tier, resource limits (already exist)

Complex logic that can't be expressed as prompts (parallel orchestration,
background health loops) becomes **tools** that any agent can call.

**Goal:** One agent class, many behaviors. Decompose big tasks, agent memory,
separation of responsibilities, cron tasks, human interaction -- all through
configuration, not code.

## Files Modified

### Go kernel
- `internal/kernel/startup.go` -- added SystemPromptFile, Tools, Workspace, Metadata fields to StartupAgent; config loading reads prompt files, resolves workspace paths
- `cmd/hivekernel/main.go` -- spawnStartupAgents merges metadata from explicit, ClawConfig, and agent.tools/agent.workspace

### Python SDK
- `sdk/python/hivekernel_sdk/tool_agent.py` -- config-driven init, tool filtering from agent.tools metadata, dynamic system prompt with identity/tool summaries, workspace+llm in ToolContext
- `sdk/python/hivekernel_sdk/loop.py` -- context overflow recovery (3 retries with force_compress)
- `sdk/python/hivekernel_sdk/memory.py` -- estimate_tokens(), dual-trigger needs_summarization (count OR tokens), force_compress()
- `sdk/python/hivekernel_sdk/tools.py` -- get_summaries() on ToolRegistry, workspace+llm on ToolContext
- `sdk/python/hivekernel_sdk/builtin_tools.py` -- added ScheduleCronTool, GetProcessInfoTool (11 total)

### New files
- `sdk/python/hivekernel_sdk/power_tools.py` -- OrchestrateTool (LLM decompose -> parallel workers -> synthesis), DelegateAsyncTool (fire-and-forget IPC)
- `sdk/python/hivekernel_sdk/workspace_tools.py` -- read_file, write_file, edit_file, list_dir, shell_exec with _safe_path sandboxing
- `sdk/python/hivekernel_sdk/web_tools.py` -- WebFetchTool (URL fetch + HTML-to-text), WebSearchTool (Brave API)
- `prompts/*.md` -- system prompt files for 8 agent roles (queen, assistant, coder, worker, orchestrator, architect, maid, github-monitor)
- `configs/startup-universal.json` -- example config using ToolAgent for all agents

### Tests
- `sdk/python/tests/test_workspace_tools.py` -- 19 tests (safe path, file CRUD, shell exec, timeout)
- `sdk/python/tests/test_power_tools.py` -- 7 tests (orchestrate flow, delegate, error handling, worker cleanup)
- `sdk/python/tests/test_web_tools.py` -- 13 tests (HTML parsing, fetch, search, error cases)
- Updated: test_builtin_tools.py (+2), test_memory.py (+5), test_tool_agent.py (+5)
- **Total: 120 tests pass**

---

## Phase 1: ToolAgent Config Enhancement

### 1.1 Extend StartupAgent config (Go)

Added fields to `StartupAgent`:
```go
SystemPromptFile string            `json:"system_prompt_file,omitempty"`
Tools            []string          `json:"tools,omitempty"`
Workspace        string            `json:"workspace,omitempty"`
Metadata         map[string]string `json:"metadata,omitempty"`
```

Config loading: if `system_prompt_file` is set, reads file content (relative to config dir). `system_prompt_file` wins over `system_prompt`.

Workspace: resolved to absolute path; creates `{workspace}/{agent-name}/` subdir.

In main.go, tools and workspace passed through metadata:
- `tools` -> `metadata["agent.tools"]` (comma-separated)
- `workspace` -> `metadata["agent.workspace"]` (resolved path)

### 1.2 Tool filtering in ToolAgent (Python)

In `on_init()`:
1. Reads `config.metadata.get("agent.tools")` -- comma-separated tool names
2. If set: collects all available tools (builtin + power + workspace + web + subclass), registers only named ones
3. If not set: registers all builtins + subclass tools (backward compatible)

### 1.3 Dynamic system prompt

Built in `_build_system_prompt()`:
```
{config.system_prompt}

## Identity
You are {config.name} (PID {self.pid}), role={role}, tier={tier}.

## Available Tools
- tool_name: tool_description
...
```

### 1.4 Workspace path + LLM in ToolContext

Added `workspace: str = ""` and `llm: Any = None` to `ToolContext`.
Set from metadata and agent's LLM client in `_make_tool_ctx()`.

---

## Phase 2: Robustness

### 2.1 Context overflow recovery

Wraps LLM call in loop.py with 3-retry logic. On RuntimeError containing
"context", "token", or "length": calls `memory.force_compress()` and retries.

### 2.2 Token-based summarization

- `estimate_tokens()`: ~2.5 chars per token heuristic
- `needs_summarization(threshold=20, max_tokens=60000)`: dual trigger
- `force_compress()`: drops oldest 50%, keeps at least last 2 messages

---

## Phase 3: Power Tools

### 3.1 schedule_cron tool
Wraps `ctx.core.add_cron()`. Parameters: name, cron_expression, target_pid, description, action.

### 3.2 orchestrate tool
Full parallel orchestration as a tool:
1. LLM decomposes task into subtasks (JSON array)
2. Spawns worker agents (role=task, tier=operational, model=mini)
3. Executes subtasks in parallel via `asyncio.gather`
4. LLM synthesizes results
5. Kills workers (in finally block for cleanup)

### 3.3 delegate_async tool
Fire-and-forget: sends `task_request` IPC message to target PID.

### 3.4 get_process_info tool
Wraps `ctx.core.get_process_info()`. Returns pid, ppid, name, role, state, tier, model.

---

## Phase 4: Workspace Tools

### 4.1 File tools
All file tools validate paths via `_safe_path()` (rejects `..` traversal outside workspace):
- `read_file(path)` -- read up to 50KB
- `write_file(path, content)` -- creates parent dirs
- `edit_file(path, old_text, new_text)` -- search/replace
- `list_dir(path=".")` -- directory listing with sizes

### 4.2 Shell exec tool
`shell_exec(command, timeout_seconds=30)` -- runs in workspace cwd, captures stdout+stderr, truncates at 50KB.

### 4.3 Web tools
- `web_fetch(url, max_length=50000)` -- HTTP GET, strips HTML tags, truncates
- `web_search(query, count=5)` -- Brave Search API (requires BRAVE_API_KEY env)

---

## Phase 5: Agent Prompt Files + Migration

### 5.1 Prompt files
8 markdown files in `prompts/`: queen, assistant, coder, worker, orchestrator, architect, maid, github-monitor. Each defines the agent's identity, job, and behavioral guidelines.

### 5.2 startup-universal.json
Example config with 5 agents (queen, assistant, coder, architect, maid), each using `hivekernel_sdk.tool_agent:ToolAgent` with different prompt files and tool sets.

### 5.3 Tests
120 tests across 8 test files. All pass. Old agent classes unchanged (no breaking changes).

---

## Implementation Order (completed)

| Step | Phase | What | Files | Status |
|------|-------|------|-------|--------|
| 1 | 1.1 | StartupAgent config extension | startup.go, main.go | DONE |
| 2 | 1.2-1.4 | Tool filtering + dynamic prompt + ToolContext | tool_agent.py, tools.py | DONE |
| 3 | 2.1-2.2 | Context overflow + token summarization | loop.py, memory.py | DONE |
| 4 | 3.1,3.4 | schedule_cron + get_process_info tools | builtin_tools.py | DONE |
| 5 | 3.2-3.3 | orchestrate + delegate_async tools | power_tools.py | DONE |
| 6 | 4.1-4.2 | File tools + shell exec | workspace_tools.py | DONE |
| 7 | 4.3 | Web tools | web_tools.py | DONE |
| 8 | 5.1-5.2 | Prompt files + universal config | prompts/*.md, configs/ | DONE |
| 9 | 5.3 | Tests | tests/ | DONE |

## Verification

1. `go build -o bin/hivekernel.exe ./cmd/hivekernel` -- OK
2. `go test ./internal/... -v` -- all pass
3. `uv run pytest sdk/python/tests/ -v` -- 120 pass (test_assistant.py failures are pre-existing, unrelated)
4. Old configs (startup-full.json) still work with old agent classes
