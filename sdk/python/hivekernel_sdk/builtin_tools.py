"""Built-in syscall tools for the agent loop."""

from __future__ import annotations

import json
from typing import Any

from .tools import Tool, ToolContext, ToolRegistry, ToolResult


class SpawnChildTool:
    """Spawn a child agent."""

    @property
    def name(self) -> str:
        return "spawn_child"

    @property
    def description(self) -> str:
        return "Spawn a new child agent with specified name, role, and configuration"

    @property
    def parameters(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "name": {"type": "string", "description": "Name for the child agent"},
                "role": {"type": "string", "description": "Role: worker, task, agent, lead"},
                "cognitive_tier": {"type": "string", "description": "Tier: strategic, tactical, operational"},
                "system_prompt": {"type": "string", "description": "System prompt for the child"},
                "model": {"type": "string", "description": "LLM model to use"},
            },
            "required": ["name", "role", "cognitive_tier"],
        }

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        pid = await ctx.core.spawn_child(
            name=args["name"],
            role=args["role"],
            cognitive_tier=args["cognitive_tier"],
            system_prompt=args.get("system_prompt", ""),
            model=args.get("model", ""),
        )
        return ToolResult(content=f"Spawned child agent with PID {pid}")


class ExecuteTaskTool:
    """Execute a task on another agent."""

    @property
    def name(self) -> str:
        return "execute_task"

    @property
    def description(self) -> str:
        return "Execute a task on another agent by PID"

    @property
    def parameters(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "target_pid": {"type": "integer", "description": "PID of the target agent"},
                "description": {"type": "string", "description": "Task description"},
                "timeout_seconds": {"type": "integer", "description": "Timeout in seconds"},
            },
            "required": ["target_pid", "description"],
        }

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        result = await ctx.core.execute_task(
            target_pid=args["target_pid"],
            description=args["description"],
            timeout_seconds=args.get("timeout_seconds", 120),
        )
        return ToolResult(content=json.dumps(result))


class SendMessageTool:
    """Send an IPC message to another agent."""

    @property
    def name(self) -> str:
        return "send_message"

    @property
    def description(self) -> str:
        return "Send an IPC message to another agent by PID"

    @property
    def parameters(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "to_pid": {"type": "integer", "description": "Target agent PID"},
                "type": {"type": "string", "description": "Message type"},
                "payload": {"type": "string", "description": "Message content"},
            },
            "required": ["to_pid", "type", "payload"],
        }

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        msg_id = await ctx.core.send_message(
            to_pid=args["to_pid"],
            type=args["type"],
            payload=args["payload"].encode("utf-8"),
        )
        return ToolResult(content=f"Message sent, id={msg_id}")


class StoreArtifactTool:
    """Store data persistently."""

    @property
    def name(self) -> str:
        return "store_artifact"

    @property
    def description(self) -> str:
        return "Store data persistently in the artifact store"

    @property
    def parameters(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "key": {"type": "string", "description": "Storage key"},
                "content": {"type": "string", "description": "Content to store"},
            },
            "required": ["key", "content"],
        }

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        art_id = await ctx.core.store_artifact(
            key=args["key"],
            content=args["content"].encode("utf-8"),
        )
        return ToolResult(content=f"Artifact stored, id={art_id}")


class GetArtifactTool:
    """Retrieve stored data."""

    @property
    def name(self) -> str:
        return "get_artifact"

    @property
    def description(self) -> str:
        return "Retrieve data from the artifact store by key"

    @property
    def parameters(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "key": {"type": "string", "description": "Storage key to retrieve"},
            },
            "required": ["key"],
        }

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        art = await ctx.core.get_artifact(key=args["key"])
        if art and hasattr(art, "content"):
            return ToolResult(content=art.content.decode("utf-8", errors="replace"))
        if isinstance(art, bytes):
            return ToolResult(content=art.decode("utf-8", errors="replace"))
        return ToolResult(content="", is_error=True)


class ListChildrenTool:
    """List child processes."""

    @property
    def name(self) -> str:
        return "list_children"

    @property
    def description(self) -> str:
        return "List all child agents of this agent"

    @property
    def parameters(self) -> dict[str, Any]:
        return {"type": "object", "properties": {}}

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        children = await ctx.core.list_children()
        return ToolResult(content=json.dumps(children))


class ListSiblingsTool:
    """List sibling processes."""

    @property
    def name(self) -> str:
        return "list_siblings"

    @property
    def description(self) -> str:
        return "List sibling agents (same parent, excluding self)"

    @property
    def parameters(self) -> dict[str, Any]:
        return {"type": "object", "properties": {}}

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        siblings = await ctx.core.list_siblings()
        return ToolResult(content=json.dumps(siblings))


class MemoryStoreTool:
    """Save to long-term memory."""

    @property
    def name(self) -> str:
        return "memory_store"

    @property
    def description(self) -> str:
        return "Save information to long-term memory for recall across sessions"

    @property
    def parameters(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "content": {"type": "string", "description": "Content to remember"},
            },
            "required": ["content"],
        }

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        key = f"agent:{ctx.pid}:memory"
        await ctx.core.store_artifact(
            key=key,
            content=args["content"].encode("utf-8"),
        )
        return ToolResult(content="Memory saved")


class MemoryRecallTool:
    """Recall from long-term memory."""

    @property
    def name(self) -> str:
        return "memory_recall"

    @property
    def description(self) -> str:
        return "Recall information from long-term memory"

    @property
    def parameters(self) -> dict[str, Any]:
        return {"type": "object", "properties": {}}

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        key = f"agent:{ctx.pid}:memory"
        art = await ctx.core.get_artifact(key=key)
        if art and hasattr(art, "content"):
            return ToolResult(content=art.content.decode("utf-8", errors="replace"))
        if isinstance(art, bytes):
            return ToolResult(content=art.decode("utf-8", errors="replace"))
        return ToolResult(content="No memory stored yet")


ALL_BUILTIN_TOOLS = [
    SpawnChildTool,
    ExecuteTaskTool,
    SendMessageTool,
    StoreArtifactTool,
    GetArtifactTool,
    ListChildrenTool,
    ListSiblingsTool,
    MemoryStoreTool,
    MemoryRecallTool,
]


def register_builtin_tools(registry: ToolRegistry) -> None:
    """Register all built-in syscall tools."""
    for cls in ALL_BUILTIN_TOOLS:
        registry.register(cls())
