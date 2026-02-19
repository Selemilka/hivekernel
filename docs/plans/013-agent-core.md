# Plan 013: AgentCore -- Tool Calling, Agent Loop, Memory

## Status: Completed

## Context

HiveKernel agents are currently reactive one-shot executors: each task triggers
one LLM call with hardcoded logic. The LLM cannot call tools, make decisions, or
iterate. There is no persistent memory between tasks. This prevents building
real-world multi-phase projects where agents need to reason, use tools, and
accumulate context.

## Goal

Build "PicoClaw-lite" natively in the Python SDK: tool calling via OpenRouter,
iterative agent loop, persistent memory via artifact store.

## Approach

5 new files (~550 lines), no modifications to existing agent files. New `ToolAgent`
base class extends `LLMAgent`. Existing agents (Queen, Worker, Orchestrator) remain
unchanged.

## Files Added

| File | Lines | Description |
|------|-------|-------------|
| `sdk/python/hivekernel_sdk/tools.py` | ~100 | Tool protocol, ToolResult, ToolContext, ToolRegistry |
| `sdk/python/hivekernel_sdk/builtin_tools.py` | ~150 | 9 syscall tools + register_builtin_tools() |
| `sdk/python/hivekernel_sdk/memory.py` | ~90 | AgentMemory (artifact-backed persistent memory) |
| `sdk/python/hivekernel_sdk/loop.py` | ~130 | AgentLoop: iterative LLM + tool execution engine |
| `sdk/python/hivekernel_sdk/tool_agent.py` | ~80 | ToolAgent base class (extends LLMAgent) |

## Files Modified

| File | Change |
|------|--------|
| `sdk/python/hivekernel_sdk/llm.py` | Added `chat_with_tools()` method (~55 lines) |
| `sdk/python/hivekernel_sdk/__init__.py` | Added 8 new exports |

## Tests Added

| File | Tests |
|------|-------|
| `sdk/python/tests/test_tools.py` | 18 tests: registry, protocol, context, schema |
| `sdk/python/tests/test_memory.py` | 13 tests: add_message, context, summarization, load/save |
| `sdk/python/tests/test_loop.py` | 9 tests: basic loop, tool calls, max iterations, errors, memory |
| `sdk/python/tests/test_builtin_tools.py` | 14 tests: each tool + schema validation |
| `sdk/python/tests/test_tool_agent.py` | 13 tests: on_init, handle_task, handle_message, get_tools |

**Total: 67 new tests, all passing.**

## Architecture

### Tool Protocol
```python
class Tool(Protocol):
    name: str
    description: str
    parameters: dict  # JSON Schema
    async def execute(ctx: ToolContext, args: dict) -> ToolResult
```

### Agent Loop Flow
```
User prompt -> Memory -> LLM (with tools) -> Tool calls?
  Yes -> Execute tools -> Record results -> Loop back to LLM
  No  -> Final response -> Save memory -> Return
```

### Built-in Tools (9)
spawn_child, execute_task, send_message, store_artifact, get_artifact,
list_children, list_siblings, memory_store, memory_recall

### Memory Persistence
Stored as artifacts: `agent:{pid}:memory`, `agent:{pid}:session`, `agent:{pid}:summary`

## What Was NOT Changed

- Queen, Worker, Orchestrator, Architect, Assistant, Coder, Maid, GitHubMonitor
- LLMClient.chat() (only added new chat_with_tools method)
- HiveAgent, LLMAgent base classes
- CoreClient, SyscallContext
- Go kernel (no changes needed)
