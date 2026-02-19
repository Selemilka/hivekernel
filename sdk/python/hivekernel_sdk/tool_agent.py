"""ToolAgent -- LLMAgent with tool calling, agent loop, and memory."""

from __future__ import annotations

import json
import logging
from typing import Any

from .builtin_tools import register_builtin_tools
from .llm_agent import LLMAgent
from .loop import AgentLoop
from .memory import AgentMemory
from .tools import Tool, ToolContext, ToolRegistry
from .types import AgentConfig, Message, MessageAck, Task, TaskResult
from .syscall import SyscallContext

logger = logging.getLogger("hivekernel.tool_agent")


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

    async def on_init(self, config: AgentConfig) -> None:
        """Initialize LLM, tools, memory, and agent loop."""
        await super().on_init(config)

        # Register built-in syscall tools
        register_builtin_tools(self.registry)

        # Register subclass custom tools
        for tool in self.get_tools():
            self.registry.register(tool)

        # Initialize memory and loop
        self.memory = AgentMemory(pid=self.pid)
        max_iter = int(config.metadata.get("max_iterations", "15"))
        self.agent_loop = AgentLoop(
            self.llm, self.registry, self.memory, max_iter
        )

    def get_tools(self) -> list[Tool]:
        """Override to add custom tools. Called during on_init."""
        return []

    async def handle_task(self, task: Task, ctx: SyscallContext) -> TaskResult:
        """Run the agent loop for a task."""
        tool_ctx = ToolContext(pid=self.pid, core=self._core, syscall=ctx)
        await self.memory.load(tool_ctx)

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
            },
        )

    async def handle_message(self, message: Message) -> MessageAck:
        """Handle IPC messages with the agent loop."""
        if message.type == "task_request":
            try:
                payload = json.loads(message.payload.decode("utf-8"))
                prompt = payload.get("task", payload.get("description", ""))
            except (json.JSONDecodeError, UnicodeDecodeError):
                prompt = message.payload.decode("utf-8", errors="replace")

            tool_ctx = ToolContext(pid=self.pid, core=self._core)
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
