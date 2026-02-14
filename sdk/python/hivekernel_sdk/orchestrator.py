"""Universal LLM Orchestrator agent for HiveKernel.

Receives a task, asks LLM to decompose it into subtasks, spawns WorkerAgents,
delegates subtasks, collects results, synthesizes a final report.

Runtime image: hivekernel_sdk.orchestrator:OrchestratorAgent
"""

import asyncio
import json

from .llm_agent import LLMAgent
from .syscall import SyscallContext
from .types import TaskResult


MAX_WORKERS = 5


class OrchestratorAgent(LLMAgent):
    """Generic LLM orchestrator: decomposes tasks, spawns workers, synthesizes."""

    async def handle_task(self, task, ctx: SyscallContext) -> TaskResult:
        description = task.params.get("task", task.description)
        max_workers = int(task.params.get("max_workers", "3"))
        max_workers = min(max_workers, MAX_WORKERS)

        await ctx.log("info", f"Orchestrator starting: {description[:80]}")
        await ctx.report_progress("Analyzing task...", 5.0)

        # --- 1. Ask LLM to decompose task into subtasks ---
        decompose_prompt = (
            f"You are a task planner. Break down the following task into "
            f"{max_workers} independent subtasks that can be worked on in parallel.\n\n"
            f"Task: {description}\n\n"
            f"Return ONLY a JSON array of strings, where each string is a clear, "
            f"self-contained subtask description. No other text."
        )

        subtasks_raw = await self.ask(decompose_prompt, max_tokens=1024, temperature=0.5)

        # Parse subtasks (handle markdown fences).
        subtasks_text = subtasks_raw.strip()
        if subtasks_text.startswith("```"):
            lines = subtasks_text.split("\n")
            subtasks_text = "\n".join(
                l for l in lines if not l.strip().startswith("```")
            )
        try:
            subtasks = json.loads(subtasks_text)
            if not isinstance(subtasks, list):
                subtasks = [str(subtasks)]
        except json.JSONDecodeError:
            # Fallback: split by newlines.
            subtasks = [
                s.strip().strip('"').strip("- ").strip("0123456789.)")
                for s in subtasks_raw.strip().split("\n")
                if s.strip() and not s.strip().startswith("```")
            ]

        subtasks = [s.strip() for s in subtasks if s.strip()][:max_workers]

        if not subtasks:
            return TaskResult(
                exit_code=1,
                output="Failed to decompose task into subtasks",
            )

        await ctx.log("info", f"Decomposed into {len(subtasks)} subtasks")
        for i, s in enumerate(subtasks):
            await ctx.log("info", f"  [{i+1}] {s[:100]}")

        await ctx.report_progress(f"Spawning {len(subtasks)} workers...", 15.0)

        # --- 2. Spawn workers ---
        worker_pids = []
        for i, subtask in enumerate(subtasks):
            pid = await ctx.spawn(
                name=f"worker-{i+1}",
                role="task",
                cognitive_tier="operational",
                model="mini",
                system_prompt=(
                    f"You are a skilled assistant working on a specific subtask. "
                    f"Your task: {subtask}"
                ),
                runtime_image="hivekernel_sdk.worker:WorkerAgent",
                runtime_type="python",
            )
            worker_pids.append(pid)
            await ctx.log("info", f"Spawned worker-{i+1} as PID {pid}")

        await ctx.report_progress("Delegating subtasks...", 25.0)

        # --- 3. Delegate subtasks to workers ---
        results = []
        for i, (subtask, wpid) in enumerate(zip(subtasks, worker_pids)):
            pct = 25.0 + (50.0 * (i + 1) / len(subtasks))
            await ctx.report_progress(
                f"Worker {i+1}/{len(subtasks)}: {subtask[:40]}...", pct,
            )

            try:
                wr = await ctx.execute_on(
                    pid=wpid,
                    description=subtask,
                    params={"subtask": subtask},
                )
                results.append({
                    "subtask": subtask,
                    "worker_pid": wpid,
                    "output": wr.output,
                    "status": "ok",
                })
            except Exception as e:
                results.append({
                    "subtask": subtask,
                    "worker_pid": wpid,
                    "output": str(e),
                    "status": "error",
                })
                await ctx.log("error", f"Worker {wpid} failed: {e}")

        await ctx.report_progress("Synthesizing results...", 80.0)

        # --- 4. Synthesize final result ---
        findings_text = "\n\n".join(
            f"### Subtask: {r['subtask']}\n{r['output']}"
            for r in results if r["status"] == "ok"
        )

        report = await self.ask(
            f"You have completed subtasks for the following main task:\n\n"
            f"**Task:** {description}\n\n"
            f"Results from each subtask:\n\n{findings_text}\n\n"
            f"Synthesize everything into a single cohesive, well-structured response. "
            f"Include key points and a brief conclusion.",
            max_tokens=4096,
            temperature=0.5,
        )

        await ctx.report_progress("Storing results...", 90.0)

        # --- 5. Store artifact ---
        report_data = {
            "task": description,
            "subtasks": subtasks,
            "results": results,
            "synthesis": report,
            "total_tokens": self.llm.total_tokens,
            "workers_spawned": len(worker_pids),
        }
        report_json = json.dumps(report_data, indent=2, ensure_ascii=False)

        safe_key = description.lower().replace(" ", "-")[:40]
        await ctx.store_artifact(
            key=f"result-{safe_key}",
            content=report_json.encode("utf-8"),
            content_type="application/json",
        )

        # --- 6. Cleanup workers ---
        # Workers are role=task and auto-exit after execute_on completes.
        # Kill any that might still be alive (safety net), in parallel.
        async def _kill_safe(wpid):
            try:
                await ctx.kill(wpid)
            except Exception:
                pass  # Already exited (expected for task role)

        await asyncio.gather(*[_kill_safe(wpid) for wpid in worker_pids])

        await ctx.log("info", f"Orchestrator done. {len(results)} subtasks, "
                       f"{self.llm.total_tokens} tokens total")

        return TaskResult(
            exit_code=0,
            output=report,
            artifacts={"total_tokens": str(self.llm.total_tokens)},
            metadata={"subtasks_count": str(len(subtasks))},
        )
