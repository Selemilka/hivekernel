"""ArchitectAgent -- strategic planner for HiveKernel.

Receives a task description, analyzes it, and produces a structured
execution plan as JSON. The plan defines groups of subtasks, their
relationships, and execution parameters.

Used by Queen when a task requires strategic planning before execution.
Spawned as role=task (auto-exits after producing plan).

Runtime image: hivekernel_sdk.architect:ArchitectAgent
"""

import json
import logging

from .llm_agent import LLMAgent
from .syscall import SyscallContext
from .types import TaskResult

logger = logging.getLogger("hivekernel.architect")


class ArchitectAgent(LLMAgent):
    """Strategic planner: analyzes task, produces structured execution plan."""

    async def handle_task(self, task, ctx: SyscallContext) -> TaskResult:
        description = task.params.get("task", task.description)
        max_workers = int(task.params.get("max_workers", "3"))

        await ctx.log("info", f"Architect analyzing: {description[:80]}")
        await ctx.report_progress("Analyzing task...", 10.0)

        # Produce structured plan via LLM.
        plan_prompt = (
            "You are a strategic architect. Analyze the following task and "
            "produce a detailed, structured execution plan.\n\n"
            f"Task: {description}\n\n"
            f"Produce a JSON plan with this exact structure:\n"
            "{{\n"
            '  "analysis": "Brief analysis of the task requirements and challenges",\n'
            '  "approach": "High-level approach chosen and why",\n'
            '  "groups": [\n'
            '    {{\n'
            '      "name": "short-group-name",\n'
            '      "subtasks": ["Detailed subtask 1", "Detailed subtask 2"],\n'
            '      "rationale": "Why these subtasks are grouped together"\n'
            '    }}\n'
            '  ],\n'
            f'  "max_workers": {max_workers},\n'
            '  "risks": "Potential failure points and mitigations",\n'
            '  "acceptance_criteria": "How to verify the task is complete"\n'
            "}}\n\n"
            "Rules:\n"
            "- Group related subtasks that benefit from shared context\n"
            "- Independent groups will run in parallel\n"
            "- Each subtask must be self-contained and actionable\n"
            f"- Maximum {max_workers} groups\n"
            "- Be specific and thorough in subtask descriptions\n"
            "Return ONLY the JSON. No other text."
        )

        await ctx.report_progress("Designing plan...", 30.0)
        plan_raw = await self.ask(plan_prompt, max_tokens=2048, temperature=0.3)

        # Validate plan is parseable JSON.
        plan_text = plan_raw.strip()
        if plan_text.startswith("```"):
            lines = plan_text.split("\n")
            plan_text = "\n".join(
                l for l in lines if not l.strip().startswith("```")
            )
            plan_text = plan_text.strip()

        try:
            plan_data = json.loads(plan_text)
            if not isinstance(plan_data, dict) or "groups" not in plan_data:
                raise ValueError("Plan missing 'groups' field")
            groups = plan_data["groups"]
            if not isinstance(groups, list) or not groups:
                raise ValueError("Plan has empty groups")
            for g in groups:
                if not isinstance(g, dict) or not g.get("subtasks"):
                    raise ValueError(f"Group missing subtasks: {g}")
        except (json.JSONDecodeError, ValueError) as e:
            await ctx.log("error", f"Plan validation failed: {e}")
            # Return fallback plan with the original task as a single subtask.
            plan_text = json.dumps({
                "analysis": "Auto-generated fallback plan",
                "approach": "Direct decomposition",
                "groups": [{"name": "main", "subtasks": [description]}],
                "max_workers": max_workers,
                "risks": "Fallback plan -- original planning failed",
                "acceptance_criteria": "Task completed",
            }, ensure_ascii=False)

        await ctx.report_progress("Storing plan...", 80.0)

        # Store plan as artifact for reference.
        try:
            await ctx.store_artifact(
                key="architect/plan",
                content=plan_text.encode("utf-8"),
                content_type="application/json",
            )
        except Exception as e:
            logger.warning("Failed to store plan artifact: %s", e)

        await ctx.log("info", f"Architect plan complete ({len(plan_text)} bytes)")
        await ctx.report_progress("Done", 100.0)

        return TaskResult(
            exit_code=0,
            output=plan_text,
            metadata={"plan_size": str(len(plan_text))},
        )
