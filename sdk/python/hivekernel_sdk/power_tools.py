"""Power tools: orchestrate and delegate_async for advanced agent coordination."""

from __future__ import annotations

import asyncio
import json
import logging
from typing import Any

from .tools import Tool, ToolContext, ToolResult

logger = logging.getLogger("hivekernel.power_tools")


class OrchestrateTool:
    """Decompose a task into parallel subtasks, spawn workers, execute, synthesize."""

    @property
    def name(self) -> str:
        return "orchestrate"

    @property
    def description(self) -> str:
        return (
            "Decompose a complex task into parallel subtasks, spawn workers, "
            "execute them, and synthesize results. Requires LLM access."
        )

    @property
    def parameters(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "task": {
                    "type": "string",
                    "description": "The task to decompose and execute in parallel",
                },
                "max_workers": {
                    "type": "integer",
                    "description": "Maximum number of parallel workers (default 3)",
                },
            },
            "required": ["task"],
        }

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        if ctx.llm is None:
            return ToolResult(
                content="orchestrate requires LLM access (ctx.llm not set)",
                is_error=True,
            )

        task = args["task"]
        max_workers = args.get("max_workers", 3)

        # Step 1: Ask LLM to decompose the task into subtasks.
        decompose_prompt = (
            f"Decompose this task into {max_workers} or fewer independent subtasks.\n"
            f"Task: {task}\n\n"
            f"Return ONLY a JSON array of strings, each being a subtask description.\n"
            f"Example: [\"subtask 1\", \"subtask 2\", \"subtask 3\"]"
        )
        try:
            raw = await ctx.llm.complete(decompose_prompt, max_tokens=1024)
            # Extract JSON from response.
            start = raw.find("[")
            end = raw.rfind("]") + 1
            if start == -1 or end == 0:
                return ToolResult(
                    content=f"LLM did not return valid JSON array for decomposition: {raw[:200]}",
                    is_error=True,
                )
            subtasks = json.loads(raw[start:end])
            if not isinstance(subtasks, list) or not subtasks:
                return ToolResult(
                    content="Decomposition returned empty or non-list result",
                    is_error=True,
                )
        except Exception as e:
            return ToolResult(
                content=f"Decomposition failed: {e}", is_error=True
            )

        # Step 2: Spawn workers and execute subtasks in parallel.
        worker_pids = []
        try:
            for i, subtask in enumerate(subtasks[:max_workers]):
                pid = await ctx.core.spawn_child(
                    name=f"orch-worker-{i}",
                    role="task",
                    cognitive_tier="operational",
                    system_prompt=f"Execute this subtask thoroughly: {subtask}",
                    model="mini",
                    runtime_image="hivekernel_sdk.tool_agent:ToolAgent",
                )
                worker_pids.append((pid, subtask))

            # Execute tasks in parallel.
            async def _run_subtask(pid: int, desc: str) -> dict:
                try:
                    return await ctx.core.execute_task(
                        target_pid=pid,
                        description=desc,
                        timeout_seconds=120,
                    )
                except Exception as e:
                    return {"exit_code": 1, "output": f"Error: {e}"}

            results = await asyncio.gather(
                *[_run_subtask(pid, desc) for pid, desc in worker_pids]
            )
        finally:
            # Step 5: Kill workers.
            for pid, _ in worker_pids:
                try:
                    await ctx.core.kill_child(pid)
                except Exception:
                    pass

        # Step 4: Synthesize results via LLM.
        synthesis_parts = []
        for i, ((pid, desc), result) in enumerate(zip(worker_pids, results)):
            output = result.get("output", "")[:2000]
            synthesis_parts.append(f"Subtask {i+1}: {desc}\nResult: {output}\n")

        synthesis_prompt = (
            f"Original task: {task}\n\n"
            f"Subtask results:\n{''.join(synthesis_parts)}\n"
            f"Synthesize these results into a coherent final answer."
        )
        try:
            synthesis = await ctx.llm.complete(synthesis_prompt, max_tokens=2048)
        except Exception as e:
            synthesis = f"Synthesis failed: {e}. Raw results: {json.dumps([r.get('output', '') for r in results])}"

        return ToolResult(content=synthesis)


class DelegateAsyncTool:
    """Fire-and-forget delegation to another agent via IPC message."""

    @property
    def name(self) -> str:
        return "delegate_async"

    @property
    def description(self) -> str:
        return "Send a task to another agent asynchronously (fire-and-forget). The result will arrive later as a message."

    @property
    def parameters(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "target_pid": {
                    "type": "integer",
                    "description": "PID of the target agent",
                },
                "task_description": {
                    "type": "string",
                    "description": "Description of the task to delegate",
                },
            },
            "required": ["target_pid", "task_description"],
        }

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        payload = json.dumps({"task": args["task_description"]}).encode("utf-8")
        msg_id = await ctx.core.send_message(
            to_pid=args["target_pid"],
            type="task_request",
            payload=payload,
        )
        return ToolResult(
            content=f"Task delegated to PID {args['target_pid']}, msg_id={msg_id}. Result will arrive later."
        )


POWER_TOOLS = [OrchestrateTool, DelegateAsyncTool]
