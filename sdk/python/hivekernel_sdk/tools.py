"""Tool infrastructure: Tool protocol, ToolResult, ToolContext, ToolRegistry."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Protocol, runtime_checkable


@dataclass
class ToolResult:
    """Result returned from a tool execution."""

    content: str
    is_error: bool = False


@runtime_checkable
class Tool(Protocol):
    """Protocol for tools that can be called by the agent loop."""

    @property
    def name(self) -> str: ...

    @property
    def description(self) -> str: ...

    @property
    def parameters(self) -> dict[str, Any]: ...

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult: ...


@dataclass
class ToolContext:
    """Unified context for tool execution.

    Tools work in both handle_task (with SyscallContext) and
    handle_message (CoreClient only) contexts.
    """

    pid: int
    core: Any  # CoreClient (always available)
    syscall: Any = None  # SyscallContext (only during handle_task)

    async def log(self, level: str, message: str) -> None:
        """Log a message via syscall or core."""
        if self.syscall is not None:
            await self.syscall.log(level, message)
        elif self.core is not None:
            await self.core.log(level, message)

    async def store_artifact(self, key: str, content: bytes) -> str:
        """Store artifact via syscall or core."""
        if self.syscall is not None:
            return await self.syscall.store_artifact(key, content)
        return await self.core.store_artifact(key, content)

    async def get_artifact(self, key: str) -> Any:
        """Get artifact via syscall or core."""
        if self.syscall is not None:
            return await self.syscall.get_artifact(key)
        return await self.core.get_artifact(key)


class ToolRegistry:
    """Registry of tools available to an agent."""

    def __init__(self) -> None:
        self._tools: dict[str, Tool] = {}

    def register(self, tool: Tool) -> None:
        """Register a tool."""
        self._tools[tool.name] = tool

    def get(self, name: str) -> Tool | None:
        """Get a tool by name."""
        return self._tools.get(name)

    async def execute(
        self, name: str, ctx: ToolContext, args: dict[str, Any]
    ) -> ToolResult:
        """Execute a tool by name. Returns error ToolResult on failure."""
        tool = self._tools.get(name)
        if tool is None:
            return ToolResult(content=f"Unknown tool: {name}", is_error=True)
        try:
            return await tool.execute(ctx, args)
        except Exception as e:
            return ToolResult(content=f"Tool {name} failed: {e}", is_error=True)

    def to_openai_schema(self) -> list[dict]:
        """Convert registered tools to OpenAI/OpenRouter tool definitions."""
        result = []
        for tool in self._tools.values():
            result.append(
                {
                    "type": "function",
                    "function": {
                        "name": tool.name,
                        "description": tool.description,
                        "parameters": tool.parameters,
                    },
                }
            )
        return result
