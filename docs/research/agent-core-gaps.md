# AgentCore: Gaps vs PicoClaw

After Plan 013 all 5 core components exist (loop, messages, registry, memory, session).
Below are the remaining gaps, grouped by priority.

## Priority 1: Robustness (agent loop crashes on long sessions)

### 1.1 Context Overflow Recovery
- **Problem**: LLM returns error when context window exceeded -- task fails
- **PicoClaw**: 2 retries + `forceCompression()` (drops oldest 50%, keeps last message)
- **Fix**: Add retry loop in `AgentLoop.run()` -- on context error, compress history, rebuild messages, retry
- **Effort**: ~30 lines in `loop.py`

### 1.2 Token-Based Summarization
- **Problem**: Summarization triggers at >20 messages regardless of token count. Short messages waste context, long messages overflow before trigger
- **PicoClaw**: Estimates tokens (2.5 chars/token), triggers at 75% of context window
- **Fix**: Add `estimate_tokens()` to `AgentMemory`, check both message count AND token estimate
- **Effort**: ~20 lines in `memory.py`

## Priority 2: Workspace Tools (agents can't build real projects without file/shell/web)

### 2.1 File Tools
- `read_file` -- read file from agent workspace
- `write_file` -- write/create file
- `edit_file` -- line-based editing (search/replace)
- `list_dir` -- list directory contents
- **Design choice**: Where is the workspace? Options:
  - (a) Per-agent temp dir on the VPS
  - (b) Artifact store (virtual filesystem via key prefixes)
  - (c) Shared workspace dir (like PicoClaw's `workspace/`)

### 2.2 Shell Exec Tool
- `exec` -- run shell command with timeout, capture stdout/stderr
- **Security**: Must sandbox. Options: chroot, nsjail, or just cwd restriction
- **PicoClaw approach**: Simple `os/exec` with timeout + output capture (50KB limit)

### 2.3 Web Tools
- `web_search` -- search via Brave/DuckDuckGo API
- `web_fetch` -- fetch URL, convert to text, truncate to size limit
- **PicoClaw approach**: Brave Search API key + readability extraction (50KB limit)

### 2.4 Cron Tool
- **Problem**: Kernel has cron engine but LLM can't schedule tasks via tool call
- **Fix**: Add `schedule_cron` tool that calls `ctx.core.add_cron()`
- **Effort**: ~15 lines (one more builtin tool)

## Priority 3: System Prompt (static vs dynamic)

### 3.1 Dynamic System Prompt Building
- **Problem**: System prompt is static string from kernel config
- **PicoClaw**: Dynamically builds: identity + available tools + bootstrap files + memory
- **Fix**: `ToolAgent.on_init()` could build prompt from: config.system_prompt + tool summaries + memory
- **Effort**: ~30 lines in `tool_agent.py`

### 3.2 Tool Listing in Prompt
- **Problem**: LLM doesn't see tool names/descriptions in system prompt (only in API schema)
- **PicoClaw**: Injects `## Available Tools\n- tool_name - description` into system prompt
- **Fix**: Add `ToolRegistry.get_summaries()`, inject into system prompt
- **Effort**: ~10 lines

## Not Needed (PicoClaw-specific, not relevant for HiveKernel)

- **Hardware I/O** (I2C, SPI) -- IoT-specific, out of scope
- **Multi-channel gateway** (Telegram, Discord, Slack) -- kernel handles routing
- **Skills system** (SKILL.md files) -- agents define tools via `get_tools()`
- **Daily notes** -- artifact store covers this use case
- **Subagent tool** -- `spawn_child` + `execute_task` already cover this

## Decision Matrix

| Item | Impact | Effort | Depends On |
|------|--------|--------|------------|
| 1.1 Context overflow recovery | High | Small | Nothing |
| 1.2 Token-based summarization | Medium | Small | Nothing |
| 2.1 File tools | High | Medium | Workspace design decision |
| 2.2 Shell exec | High | Medium | Security design decision |
| 2.3 Web tools | Medium | Small | API key management |
| 2.4 Cron tool | Low | Small | Nothing |
| 3.1 Dynamic system prompt | Medium | Small | Nothing |
| 3.2 Tool listing in prompt | Low | Small | Nothing |
