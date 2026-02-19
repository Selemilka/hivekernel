"""ToolAgent -- LLMAgent with tool calling, agent loop, and memory."""

from __future__ import annotations

import json
import logging
import uuid
from typing import Any

from .builtin_tools import ALL_BUILTIN_TOOLS, register_builtin_tools
from .llm_agent import LLMAgent
from .loop import AgentLoop
from .memory import AgentMemory
from .tools import Tool, ToolContext, ToolRegistry
from .types import AgentConfig, Message, MessageAck, Task, TaskResult
from .syscall import SyscallContext

logger = logging.getLogger("hivekernel.tool_agent")


def _get_all_available_tools() -> list[Tool]:
    """Collect all tool instances from builtin + extension modules."""
    tools: list[Tool] = [cls() for cls in ALL_BUILTIN_TOOLS]
    # Import extension tool modules; each defines a list for registration.
    try:
        from .power_tools import POWER_TOOLS
        tools.extend(cls() for cls in POWER_TOOLS)
    except ImportError:
        pass
    try:
        from .workspace_tools import WORKSPACE_TOOLS
        tools.extend(cls() for cls in WORKSPACE_TOOLS)
    except ImportError:
        pass
    try:
        from .web_tools import WEB_TOOLS
        tools.extend(cls() for cls in WEB_TOOLS)
    except ImportError:
        pass
    return tools


class ToolAgent(LLMAgent):
    """LLMAgent with built-in tool calling, agent loop, and persistent memory.

    Extends LLMAgent with:
    - ToolRegistry with built-in syscall tools
    - AgentMemory for persistent state across tasks
    - AgentLoop for iterative LLM + tool execution

    Subclass and override get_tools() to add custom tools.
    Existing agents (Queen, Worker, etc.) are unchanged.
    """

    def __init__(self) -> None:
        super().__init__()
        self.registry: ToolRegistry = ToolRegistry()
        self.memory: AgentMemory | None = None
        self.agent_loop: AgentLoop | None = None
        self._workspace: str = ""

    async def on_init(self, config: AgentConfig) -> None:
        """Initialize LLM, tools, memory, and agent loop."""
        await super().on_init(config)

        # Read workspace from metadata.
        self._workspace = config.metadata.get("agent.workspace", "")

        # Determine which tools to register.
        tools_filter = config.metadata.get("agent.tools", "")
        if tools_filter:
            # Selective: only register tools named in the comma-separated list.
            allowed = {t.strip() for t in tools_filter.split(",") if t.strip()}
            all_tools = _get_all_available_tools() + self.get_tools()
            for tool in all_tools:
                if tool.name in allowed:
                    self.registry.register(tool)
            registered = set(self.registry._tools.keys())
            missing = allowed - registered
            if missing:
                logger.warning("Requested tools not found: %s", ", ".join(sorted(missing)))
        else:
            # Default: register all builtin tools + subclass tools.
            register_builtin_tools(self.registry)
            for tool in self.get_tools():
                self.registry.register(tool)

        # Build dynamic system prompt.
        self._system_prompt = self._build_system_prompt(config)

        # Initialize memory and loop.
        self.memory = AgentMemory(pid=self.pid)
        max_iter = int(config.metadata.get("max_iterations", "15"))
        self.agent_loop = AgentLoop(
            self.llm, self.registry, self.memory, max_iter
        )

    def _build_system_prompt(self, config: AgentConfig) -> str:
        """Build dynamic system prompt with identity and tool summaries."""
        parts: list[str] = []

        # Base prompt (from config / prompt file).
        base = config.system_prompt.strip()
        if base:
            parts.append(base)

        # Identity section.
        tier = config.metadata.get("cognitive_tier", "operational")
        role = config.metadata.get("role", "agent")
        identity = (
            f"\n## Identity\n"
            f"You are {config.name} (PID {self.pid}), role={role}, tier={tier}."
        )
        parts.append(identity)

        # Available tools section.
        summaries = self.registry.get_summaries()
        if summaries:
            parts.append(f"\n## Available Tools\n{summaries}")

        return "\n".join(parts)

    def get_tools(self) -> list[Tool]:
        """Override to add custom tools. Called during on_init."""
        return []

    def _make_tool_ctx(self, syscall: Any = None) -> ToolContext:
        """Create a ToolContext with workspace and LLM populated."""
        return ToolContext(
            pid=self.pid,
            core=self._core,
            syscall=syscall,
            workspace=self._workspace,
            llm=self.llm,
        )

    async def handle_task(self, task: Task, ctx: SyscallContext) -> TaskResult:
        """Run the agent loop for a task."""
        tool_ctx = self._make_tool_ctx(syscall=ctx)
        await self.memory.load(tool_ctx)

        # Extract or generate trace_id for event correlation.
        trace_id = task.params.get("trace_id", "") or str(uuid.uuid4())

        prompt = task.params.get("task", task.description)
        result = await self.agent_loop.run(
            prompt=prompt,
            ctx=tool_ctx,
            system_prompt=self._system_prompt,
        )

        return TaskResult(
            exit_code=0,
            output=result.content,
            metadata={
                "iterations": str(result.iterations),
                "tool_calls": str(result.tool_calls_total),
                "tokens_used": str(result.total_tokens),
                "prompt_tokens": str(result.prompt_tokens),
                "completion_tokens": str(result.completion_tokens),
                "llm_calls": str(result.llm_calls),
                "latency_ms": str(result.total_latency_ms),
            },
        )

    async def handle_message(self, message: Message) -> MessageAck:
        """Handle IPC messages with the agent loop."""
        if message.type in ("task_request", "cron_task"):
            try:
                payload = json.loads(message.payload.decode("utf-8"))
                prompt = payload.get("task", payload.get("description", ""))
            except (json.JSONDecodeError, UnicodeDecodeError):
                prompt = message.payload.decode("utf-8", errors="replace")

            tool_ctx = self._make_tool_ctx()
            await self.memory.load(tool_ctx)

            result = await self.agent_loop.run(
                prompt=prompt,
                ctx=tool_ctx,
                system_prompt=self._system_prompt,
            )

            # Reply with result
            await self.reply_to(
                message,
                json.dumps({"output": result.content}).encode("utf-8"),
            )
            return MessageAck(
                message_id=message.message_id,
                status=MessageAck.ACK_ACCEPTED,
                reply=result.content[:200],
            )

        return await super().handle_message(message)
