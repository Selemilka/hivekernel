"""QueenAgent -- local coordinator daemon for HiveKernel.

Receives tasks from King/dashboard, assesses complexity, decides strategy:
- Simple task -> spawn task-role worker, single LLM call, collect result
- Complex task -> spawn lead (orchestrator), full decomposition pipeline

Stays alive between tasks (daemon role). Manages child lifecycle.

Runtime image: hivekernel_sdk.queen:QueenAgent
"""

import json
import logging

from .llm_agent import LLMAgent
from .syscall import SyscallContext
from .types import TaskResult

logger = logging.getLogger("hivekernel.queen")


class QueenAgent(LLMAgent):
    """Local coordinator daemon. Receives tasks, decides execution strategy."""

    def __init__(self):
        super().__init__()
        self._active_leads: dict[int, str] = {}  # pid -> task description

    async def handle_task(self, task, ctx: SyscallContext) -> TaskResult:
        description = task.params.get("task", task.description)
        self._max_workers = task.params.get("max_workers", "3")
        if not description.strip():
            return TaskResult(exit_code=1, output="Empty task description")

        await ctx.log("info", f"Queen received task: {description[:100]}")
        await ctx.report_progress("Assessing task complexity...", 5.0)

        # 1. Assess complexity.
        complexity = await self._assess_complexity(description)
        await ctx.log("info", f"Task complexity: {complexity}")

        # 2. Execute based on complexity.
        if complexity == "simple":
            result = await self._handle_simple(description, ctx)
        else:
            result = await self._handle_complex(description, ctx)

        # 3. Store result artifact.
        await ctx.report_progress("Storing result...", 95.0)
        safe_key = description.lower().replace(" ", "-")[:40]
        try:
            await ctx.store_artifact(
                key=f"queen-result-{safe_key}",
                content=json.dumps({
                    "task": description,
                    "complexity": complexity,
                    "output": result.output[:4000],
                    "exit_code": result.exit_code,
                }, ensure_ascii=False).encode("utf-8"),
                content_type="application/json",
            )
        except Exception as e:
            logger.warning("Failed to store artifact: %s", e)

        await ctx.log("info", f"Queen task done (exit={result.exit_code})")
        return result

    async def _assess_complexity(self, description: str) -> str:
        """Ask LLM whether task is simple or complex."""
        try:
            response = await self.ask(
                "You are a task complexity assessor. Determine if the following task "
                "is SIMPLE (can be done in a single step by one agent) or COMPLEX "
                "(needs decomposition into multiple subtasks with separate workers).\n\n"
                f"Task: {description}\n\n"
                "Reply with ONLY a JSON object: {\"complexity\": \"simple\"} or "
                "{\"complexity\": \"complex\"}. No other text.",
                max_tokens=64,
                temperature=0.0,
            )
            text = response.strip()
            if text.startswith("```"):
                lines = text.split("\n")
                text = "\n".join(l for l in lines if not l.strip().startswith("```"))
            data = json.loads(text)
            complexity = data.get("complexity", "complex")
            if complexity in ("simple", "complex"):
                return complexity
        except Exception as e:
            logger.warning("Complexity assessment failed: %s, defaulting to complex", e)
        return "complex"

    async def _handle_simple(self, description: str, ctx: SyscallContext) -> TaskResult:
        """Simple path: spawn a task-role worker, execute, collect result."""
        await ctx.report_progress("Spawning task worker...", 15.0)
        await ctx.log("info", "Simple strategy: spawning task worker")

        worker_pid = await ctx.spawn(
            name="queen-task",
            role="task",
            cognitive_tier="operational",
            model="mini",
            system_prompt=(
                "You are a skilled assistant. Complete the given task "
                "thoroughly and concisely."
            ),
            runtime_image="hivekernel_sdk.worker:WorkerAgent",
            runtime_type="python",
        )
        await ctx.log("info", f"Task worker spawned: PID {worker_pid}")
        await ctx.report_progress("Executing task...", 30.0)

        try:
            result = await ctx.execute_on(
                pid=worker_pid,
                description=description,
                params={"subtask": description},
                timeout_seconds=120,
            )
            # Worker auto-exits (role=task, Phase 1).
            return TaskResult(
                exit_code=result.exit_code,
                output=result.output,
                artifacts=result.artifacts,
                metadata={**result.metadata, "strategy": "simple"},
            )
        except Exception as e:
            await ctx.log("error", f"Task worker failed: {e}")
            # Try to kill worker if still alive.
            try:
                await ctx.kill(worker_pid)
            except Exception:
                pass
            return TaskResult(exit_code=1, output=f"Task execution failed: {e}")

    async def _handle_complex(self, description: str, ctx: SyscallContext) -> TaskResult:
        """Complex path: spawn lead (orchestrator), delegate, collect, cleanup."""
        await ctx.report_progress("Spawning orchestrator...", 15.0)
        await ctx.log("info", "Complex strategy: spawning orchestrator lead")

        lead_pid = await ctx.spawn(
            name="queen-lead",
            role="lead",
            cognitive_tier="tactical",
            model="sonnet",
            system_prompt=(
                "You are a task orchestrator. You decompose tasks, "
                "delegate to workers, and synthesize results."
            ),
            runtime_image="hivekernel_sdk.orchestrator:OrchestratorAgent",
            runtime_type="python",
        )
        self._active_leads[lead_pid] = description
        await ctx.log("info", f"Orchestrator lead spawned: PID {lead_pid}")
        await ctx.report_progress("Lead working on task...", 25.0)

        try:
            result = await ctx.execute_on(
                pid=lead_pid,
                description=description,
                params={"task": description, "max_workers": self._max_workers},
                timeout_seconds=300,
            )
            return TaskResult(
                exit_code=result.exit_code,
                output=result.output,
                artifacts=result.artifacts,
                metadata={**result.metadata, "strategy": "complex"},
            )
        except Exception as e:
            await ctx.log("error", f"Orchestrator lead failed: {e}")
            return TaskResult(exit_code=1, output=f"Task execution failed: {e}")
        finally:
            # Kill lead after task completes (Queen manages lead lifecycle).
            self._active_leads.pop(lead_pid, None)
            try:
                await ctx.kill(lead_pid)
                await ctx.log("info", f"Lead PID {lead_pid} killed after task")
            except Exception:
                pass
