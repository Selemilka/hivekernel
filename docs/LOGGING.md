# Logging

HiveKernel has three logging subsystems, each serving a different purpose:

| Subsystem | What it captures | Format | Location |
|-----------|-----------------|--------|----------|
| **Kernel log** | Go slog messages (startup, errors, runtime) | Text (console) + JSON (file) | stderr / `--log-file` path |
| **Event log** | Process lifecycle, IPC, LLM/tool call summaries | JSONL | `logs/events-YYYYMMDD-HHMMSS.jsonl` |
| **Dialog log** | Full LLM request/response payloads | JSONL | `logs/dialogs/YYYYMMDD-HHMMSS.jsonl` |

All log files live under `logs/` (gitignored).

---

## Kernel Log

Standard Go structured logging via `slog`. Every Go component creates a logger with `hklog.For("component-name")`.

### Log Levels

| Level | Flag value | Description |
|-------|-----------|-------------|
| DEBUG | `debug` | Detailed diagnostics (gRPC calls, message routing) |
| INFO | `info` | Normal operation (startup, agent spawn, task completion) |
| WARN | `warn` | Recoverable issues (agent health, missing config) |
| ERROR | `error` | Failures (spawn errors, gRPC failures) |

Default: `info`.

### Configuration

```bash
# Console only (default)
hivekernel.exe --listen :50051

# Set log level
hivekernel.exe --log-level debug

# Console + JSON file
hivekernel.exe --log-level info --log-file logs/kernel.json
```

`--log-file` opens the file in append mode (`O_CREATE|O_WRONLY|O_APPEND`). Each line is a JSON object with `time`, `level`, `msg`, and structured attributes.

### slog-to-EventLog Bridge

All kernel slog messages at INFO+ are also forwarded to the Event Log as `"log"` events. This is how kernel-side messages (Go) appear in the dashboard and event JSONL alongside agent-side events (Python).

---

## Event Log

Structured audit trail of **everything that happens** in the kernel. Written by both Go core (process lifecycle) and Python agents (via `log` syscall). One file per kernel session.

### File

```
logs/events-YYYYMMDD-HHMMSS.jsonl
```

Timestamp is kernel start time. New file on each kernel restart.

### Event Types

| Type | Source | When |
|------|--------|------|
| `spawned` | Kernel | Process registered in the tree |
| `state_changed` | Kernel | Process state transition (idle -> running, etc.) |
| `removed` | Kernel | Process unregistered |
| `log` | Kernel + Agents | Application log message |
| `message_sent` | Kernel | IPC message added to inbox |
| `message_delivered` | Kernel | IPC message pushed to agent via gRPC |
| `llm_call` | Agent | LLM API call completed (summary only) |
| `tool_call` | Agent | Tool executed |
| `cron_executed` | Kernel | Scheduled cron job fired |

### Record Structure

```json
{
  "seq": 42,
  "ts": "2026-02-20T13:34:34.123456",
  "type": "llm_call",
  "pid": 3,
  "ppid": 2,
  "name": "worker-1",
  "role": "worker",
  "tier": "operational",
  "model": "mini",
  "state": "running",
  "old_state": "",
  "new_state": "",
  "level": "info",
  "message": "LLM call: google/gemini-2.5-flash",
  "trace_id": "a1b2c3d4",
  "trace_span": "",
  "message_id": "",
  "reply_to": "",
  "payload_preview": "",
  "fields": {
    "model": "google/gemini-2.5-flash",
    "prompt_tokens": "1204",
    "completion_tokens": "387",
    "total_tokens": "1591",
    "latency_ms": "2341.5",
    "iteration": "1",
    "prompt_preview": "You are a skilled assistant...",
    "response_preview": "Here is the result..."
  }
}
```

All fields except `seq`, `ts`, `type`, `pid` are `omitempty` -- absent when not applicable.

**Key limitation:** `llm_call` events store only 500-char previews of prompt and response in `fields.prompt_preview` / `fields.response_preview`. For full message history, use the Dialog Log.

### In-Memory Ring Buffer

The event log keeps the last 4096 events in memory. When the buffer overflows, the oldest 50% is trimmed. The dashboard uses `SubscribeSince(seq)` to replay past events and stream new ones in real-time.

---

## Dialog Log

Full LLM request/response logging. Captures every message sent to and received from the LLM API, with complete content -- no truncation.

### Purpose

- Debug agent behavior by replaying exact LLM conversations
- Audit token usage per agent and per trace
- Analyze prompt quality and response patterns
- Cost tracking (usage includes cost from OpenRouter)
- Look up server-side generation details via `generation_id`

### File

```
logs/dialogs/YYYYMMDD-HHMMSS.jsonl
```

**All agents in a session share one file.** The timestamp comes from the `HIVE_SESSION_TS` environment variable, which the kernel sets on startup and child processes inherit automatically.

If an agent runs outside the kernel (no `HIVE_SESSION_TS`), it creates its own file with the current timestamp.

### What Gets Logged

Every call to `LLMClient.chat()` and `LLMClient.chat_with_tools()` is logged. This captures ALL LLM interactions across the system:

- Queen complexity assessment (LLM fallback)
- Orchestrator task decomposition
- Orchestrator result synthesis
- Worker/ToolAgent agent loop iterations (each LLM turn)
- Architect plan generation
- Any direct `self.ask()` / `self.chat()` call

### Full Record Example

Each line is a JSON object. Here is a `chat_with_tools` call during an agent loop iteration:

```json
{
  "ts": "2026-02-20T13:11:04.962607",
  "pid": 3,
  "agent": "worker-1",
  "trace_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "generation_id": "gen-1740050465-abc123defg",
  "provider": "Anthropic",
  "model": "anthropic/claude-sonnet-4-5",
  "method": "chat_with_tools",
  "request": {
    "messages": [
      {"role": "system", "content": "You are a skilled assistant working on..."},
      {"role": "user", "content": "Research quantum computing advances in 2025"},
      {"role": "assistant", "content": "", "tool_calls": [
        {"id": "toolu_abc", "type": "function", "function": {"name": "web_search", "arguments": "{\"query\": \"quantum computing 2025\"}"}}
      ]},
      {"role": "tool", "tool_call_id": "toolu_abc", "content": "Results: ..."},
      {"role": "user", "content": "Continue with the next step."}
    ],
    "tools": [
      {
        "type": "function",
        "function": {
          "name": "spawn_child",
          "description": "Spawn a new child process in the HiveKernel process tree.",
          "parameters": {
            "type": "object",
            "properties": {
              "name": {"type": "string", "description": "Process name"},
              "role": {"type": "string", "enum": ["task", "worker", "lead"]},
              "model": {"type": "string"}
            },
            "required": ["name"]
          }
        }
      },
      {
        "type": "function",
        "function": {
          "name": "store_artifact",
          "description": "Store binary artifact in the kernel artifact store.",
          "parameters": {"...": "..."}
        }
      }
    ],
    "max_tokens": 4096,
    "temperature": 0.7
  },
  "response": {
    "content": "Based on my research, here are the key quantum computing advances...",
    "tool_calls": [],
    "finish_reason": "stop",
    "reasoning": null,
    "system_fingerprint": "fp-abc123def456"
  },
  "usage": {
    "prompt_tokens": 2017,
    "completion_tokens": 547,
    "total_tokens": 2564,
    "cost": 0.006885,
    "is_byok": false,
    "prompt_tokens_details": {
      "cached_tokens": 1200,
      "cache_write_tokens": 0
    },
    "completion_tokens_details": {
      "reasoning_tokens": 0
    }
  },
  "latency_ms": 9477.7
}
```

### Top-Level Fields

| Field | Type | Description |
|-------|------|-------------|
| `ts` | string | ISO 8601 timestamp of the LLM call (local agent time) |
| `pid` | int | HiveKernel Process ID of the calling agent |
| `agent` | string | Agent name (e.g. `"queen@vps1"`, `"worker-1"`) |
| `trace_id` | string | Our trace ID linking calls across agents within one user task. UUID for IPC tasks, propagated from parent for Execute stream |
| `generation_id` | string | **OpenRouter server-side ID** (e.g. `"gen-1740050465-abc123defg"`). Can be used to look up generation details via `GET /api/v1/generation?id=<ID>` |
| `provider` | string | Which provider served the request (e.g. `"Anthropic"`, `"Google"`). Returned by OpenRouter when available |
| `model` | string | Resolved OpenRouter model ID (e.g. `"anthropic/claude-sonnet-4-5"`, `"google/gemini-2.5-flash"`) |
| `method` | string | `"chat"` (plain text completion) or `"chat_with_tools"` (function calling) |
| `request` | object | What was sent to the LLM (see below) |
| `response` | object | What the LLM returned (see below) |
| `usage` | object | Token counts and cost from OpenRouter (see below) |
| `latency_ms` | float | Wall-clock time from HTTP request start to response received, in milliseconds |

### `request` Object

Contains the exact payload sent to OpenRouter's `POST /api/v1/chat/completions`.

| Field | Type | Present | Description |
|-------|------|---------|-------------|
| `messages` | array | Always | Full conversation history. Each element is a message object (see below) |
| `tools` | array/null | Always | Tool definitions in OpenAI function calling format. `null` for plain `chat()` calls |
| `max_tokens` | int | Always | Maximum tokens the model may generate. Default: 4096 |
| `temperature` | float | Always | Sampling temperature. Default: 0.7 |

#### Message Objects in `request.messages`

Messages follow the OpenAI chat format. The conversation history grows with each agent loop iteration:

| Role | Fields | When |
|------|--------|------|
| `system` | `role`, `content` | First message if system prompt is set. Contains agent identity, tools summary, task context |
| `user` | `role`, `content` | User prompts, tool results formatted as text, "Continue" prompts from agent loop |
| `assistant` | `role`, `content`, `tool_calls` | Previous LLM responses. `content` can be empty string when model only made tool calls |
| `tool` | `role`, `tool_call_id`, `content` | Tool execution results. `tool_call_id` references the tool call it responds to |

#### Tool Definitions in `request.tools`

Each tool follows the OpenAI function calling schema:

```json
{
  "type": "function",
  "function": {
    "name": "spawn_child",
    "description": "Spawn a new child process in the HiveKernel process tree.",
    "parameters": {
      "type": "object",
      "properties": {
        "name": {"type": "string", "description": "Process name"},
        "role": {"type": "string", "enum": ["task", "worker", "lead"]}
      },
      "required": ["name"]
    }
  }
}
```

Typical ToolAgent has 10-15 tools (syscall wrappers: spawn, execute, send_message, store/get artifact, memory, cron, etc. + power tools + workspace tools).

### `response` Object

What the model returned. Structure differs slightly between `chat` and `chat_with_tools`.

| Field | Type | Present | Description |
|-------|------|---------|-------------|
| `content` | string | Always | Model's text response. Empty string `""` when model only made tool calls |
| `tool_calls` | array | `chat_with_tools` only | Tool invocations requested by the model (see below) |
| `finish_reason` | string | Always | Why the model stopped generating (see below) |
| `reasoning` | string/null | Always | Extended thinking / chain-of-thought output. `null` if model did not use reasoning. Populated by models that support reasoning (e.g. Claude with `reasoning.effort`) |
| `system_fingerprint` | string/null | Always | Provider's system configuration hash. Can be `null` |

#### `response.finish_reason` Values

OpenRouter normalizes all providers to these values:

| Value | Meaning |
|-------|---------|
| `"stop"` | Model finished generating naturally |
| `"tool_calls"` | Model wants to call one or more tools |
| `"length"` | Hit `max_tokens` limit |
| `"content_filter"` | Response blocked by content filter |
| `"error"` | Provider-side error during generation |

#### `response.tool_calls` Array

Each tool call the model wants to execute:

```json
{
  "id": "toolu_vrtx_01JMC...",
  "type": "function",
  "function": {
    "name": "store_artifact",
    "arguments": "{\"key\": \"result-summary\", \"content\": \"...\"}"
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique ID for this tool call. Used to match `tool` role messages back to this call |
| `type` | string | Always `"function"` |
| `function.name` | string | Tool name (matches `request.tools[].function.name`) |
| `function.arguments` | string | JSON-encoded arguments. Must be parsed separately |

### `usage` Object

Token counts and cost as reported by OpenRouter. Passed through as-is from the API response.

| Field | Type | Description |
|-------|------|-------------|
| `prompt_tokens` | int | Tokens in the request (system + messages + tools) |
| `completion_tokens` | int | Tokens generated by the model |
| `total_tokens` | int | `prompt_tokens + completion_tokens` |
| `cost` | float | Total cost in USD for this call |
| `is_byok` | bool | Whether Bring Your Own Key was used |

#### `usage.prompt_tokens_details` (when present)

| Field | Type | Description |
|-------|------|-------------|
| `cached_tokens` | int | Tokens served from prompt cache (saves cost). Large with repeated system prompts |
| `cache_write_tokens` | int | Tokens written to cache on this call |
| `audio_tokens` | int | Audio input tokens (if multimodal) |

#### `usage.completion_tokens_details` (when present)

| Field | Type | Description |
|-------|------|-------------|
| `reasoning_tokens` | int | Tokens used for extended thinking / chain-of-thought. Not included in `content` |
| `accepted_prediction_tokens` | int | Speculative decoding: accepted tokens |
| `rejected_prediction_tokens` | int | Speculative decoding: rejected tokens |

### OpenRouter Generation Lookup

Each record has a `generation_id` (e.g. `"gen-1740050465-abc123defg"`). This is OpenRouter's server-side identifier. You can use it to retrieve detailed generation metadata:

```bash
curl -s "https://openrouter.ai/api/v1/generation?id=gen-1740050465-abc123defg" \
  -H "Authorization: Bearer $OPENROUTER_API_KEY" | jq .
```

The generation endpoint returns server-side data not available in the chat response:

| Field | Description |
|-------|-------------|
| `provider_name` | Which provider actually served the request (e.g. `"Anthropic"`) |
| `total_cost` | Exact cost in USD |
| `upstream_inference_cost` | What the provider charged OpenRouter |
| `cache_discount` | Caching discount applied |
| `latency` | Server-side total latency (ms) |
| `generation_time` | Server-side generation time (ms), excludes network |
| `moderation_latency` | Time spent on content moderation (ms) |
| `native_tokens_prompt` / `native_tokens_completion` | Provider's own token counts (may differ from OpenRouter's) |
| `native_tokens_reasoning` | Provider's reasoning token count |
| `native_tokens_cached` | Provider's cached token count |
| `native_finish_reason` | Provider's raw finish reason (before normalization) |
| `streamed` | Whether the request used streaming |
| `cancelled` | Whether generation was cancelled |

This is useful for cost auditing and debugging provider-specific behavior.

### Trace ID Propagation

The `trace_id` field links all LLM calls belonging to the same user task across multiple agents:

```
User task -> Queen (trace_id=X) -> Orchestrator (trace_id=X) -> Worker-1 (trace_id=X)
                                                              -> Worker-2 (trace_id=X)
```

A ToolAgent's agent loop may make 5-15 LLM calls per task (tool call iterations). All of them share the same `trace_id`, so you can reconstruct the full conversation:

```bash
# PowerShell -- reconstruct one task's full dialog
Get-Content logs/dialogs/20260220-133434.jsonl |
  ForEach-Object { $_ | ConvertFrom-Json } |
  Where-Object { $_.trace_id -eq "a1b2c3d4" } |
  ForEach-Object { "$($_.ts) [$($_.agent)] $($_.method) -> $($_.response.finish_reason) ($($_.usage.total_tokens) tokens)" }
```

```bash
# jq -- same thing
jq -r 'select(.trace_id == "a1b2c3d4") |
  "\(.ts) [\(.agent)] \(.method) -> \(.response.finish_reason) (\(.usage.total_tokens) tokens)"' \
  logs/dialogs/20260220-133434.jsonl
```

### `chat` vs `chat_with_tools` Records

| Aspect | `method: "chat"` | `method: "chat_with_tools"` |
|--------|---|---|
| Used by | `LLMClient.chat()`, `LLMClient.complete()`, `LLMAgent.ask()` | `AgentLoop` (tool calling iterations) |
| `request.tools` | `null` | Array of tool definitions |
| `response.tool_calls` | Absent | Array (empty if `finish_reason: "stop"`) |
| Typical callers | Queen assessment, Orchestrator decompose/synthesize, Architect | ToolAgent, Worker (agent loop) |
| Messages pattern | Usually 1-2 messages (system + user prompt) | Growing conversation (system + user + assistant + tool + ...) |

---

## Event Log vs Dialog Log

| | Event Log | Dialog Log |
|-|-----------|------------|
| **Scope** | All kernel events | LLM calls only |
| **LLM content** | 500-char preview | Full messages, no truncation |
| **Written by** | Go kernel + Python agents | Python LLM client |
| **Dashboard** | Yes (real-time stream) | No (file analysis) |
| **Use case** | Monitoring, debugging flow | Prompt engineering, cost audit |

They complement each other: the event log gives the big picture (what happened and when), the dialog log gives the full detail (exactly what was said to the LLM).

---

## Directory Structure

```
logs/
  events-20260220-131035.jsonl    # Event log (session 1)
  events-20260220-142200.jsonl    # Event log (session 2)
  dialogs/
    20260220-131035.jsonl         # Dialog log (session 1)
    20260220-142200.jsonl         # Dialog log (session 2)
```

The `logs/` directory and `logs/dialogs/` subdirectory are created automatically on first use. All files under `logs/` are gitignored.

---

## Working with Log Files

### View latest dialog log (PowerShell)

```powershell
# Last 5 entries, showing agent + model + token count
Get-Content (Get-ChildItem logs/dialogs/*.jsonl | Sort-Object LastWriteTime | Select-Object -Last 1).FullName |
  Select-Object -Last 5 |
  ForEach-Object {
    $j = $_ | ConvertFrom-Json
    "$($j.ts) | PID $($j.pid) $($j.agent) | $($j.model) | $($j.usage.total_tokens) tokens | $($j.latency_ms)ms"
  }
```

### Count LLM calls per agent

```powershell
Get-Content logs/dialogs/20260220-131035.jsonl |
  ForEach-Object { ($_ | ConvertFrom-Json).agent } |
  Group-Object | Sort-Object Count -Descending |
  Format-Table Name, Count
```

### Total tokens per session (jq)

```bash
jq -s '[.[].usage.total_tokens] | add' logs/dialogs/20260220-131035.jsonl
```

### Filter by trace ID (jq)

```bash
jq 'select(.trace_id == "TARGET_ID") | {ts, agent, method, model, tokens: .usage.total_tokens}' \
  logs/dialogs/20260220-131035.jsonl
```
