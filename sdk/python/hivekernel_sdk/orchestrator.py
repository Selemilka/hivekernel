"""Universal LLM Orchestrator agent for HiveKernel.

Receives a task, asks LLM to decompose it into grouped subtasks, spawns a
worker pool, delegates subtasks (related subtasks to the same worker for
accumulated context), collects results, synthesizes a final report.

Phase 4: Worker reuse -- workers are role=worker (not task), stay alive
between subtasks within a group, and accumulate context. Groups execute in
parallel, subtasks within a group execute sequentially on the same worker.

Runtime image: hivekernel_sdk.orchestrator:OrchestratorAgent
"""

import asyncio
import json
import logging

from .llm_agent import LLMAgent
from .syscall import SyscallContext
from .types import TaskResult

logger = logging.getLogger("hivekernel.orchestrator")

MAX_WORKERS = 5


class OrchestratorAgent(LLMAgent):
    """Generic LLM orchestrator: decomposes, groups, spawns worker pool, synthesizes."""

    async def handle_task(self, task, ctx: SyscallContext) -> TaskResult:
        description = task.params.get("task", task.description)
        max_workers = int(task.params.get("max_workers", "3"))
        max_workers = min(max_workers, MAX_WORKERS)

        await ctx.log("info", f"Orchestrator starting: {description[:80]}")
        await ctx.report_progress("Analyzing task...", 5.0)

        # --- 1. Decompose + group subtasks ---
        groups = await self._decompose_and_group(description, max_workers)

        if not groups:
            return TaskResult(
                exit_code=1,
                output="Failed to decompose task into subtasks",
            )

        total_subtasks = sum(len(g["subtasks"]) for g in groups)
        await ctx.log("info", f"Decomposed into {len(groups)} groups "
                       f"({total_subtasks} subtasks)")
        for g in groups:
            await ctx.log("info", f"  Group '{g['name']}': {len(g['subtasks'])} subtask(s)")

        await ctx.report_progress(f"Spawning {len(groups)} workers...", 15.0)

        # --- 2. Spawn worker pool (one per group, role=worker for reuse) ---
        worker_pids = []
        for i, group in enumerate(groups):
            pid = await ctx.spawn(
                name=f"worker-{i+1}",
                role="worker",  # Stays alive for reuse within group
                cognitive_tier="operational",
                model="mini",
                system_prompt=(
                    f"You are a skilled assistant working on the '{group['name']}' "
                    f"aspect of a larger task. Complete each subtask thoroughly."
                ),
                runtime_image="hivekernel_sdk.worker:WorkerAgent",
                runtime_type="python",
            )
            worker_pids.append(pid)
            await ctx.log("info", f"Spawned worker-{i+1} as PID {pid} "
                           f"for group '{group['name']}'")

        await ctx.report_progress("Delegating subtasks...", 25.0)

        # --- 3. Execute groups in parallel, subtasks within group sequentially ---
        completed = [0]  # mutable counter for progress tracking

        async def run_group(group, wpid):
            """Execute all subtasks in a group on the same worker."""
            results = []
            for subtask in group["subtasks"]:
                try:
                    wr = await ctx.execute_on(
                        pid=wpid,
                        description=subtask,
                        params={"subtask": subtask},
                    )
                    results.append({
                        "subtask": subtask,
                        "worker_pid": wpid,
                        "group": group["name"],
                        "output": wr.output,
                        "status": "ok",
                        "tokens": wr.metadata.get("tokens_used", "0"),
                    })
                except Exception as e:
                    results.append({
                        "subtask": subtask,
                        "worker_pid": wpid,
                        "group": group["name"],
                        "output": str(e),
                        "status": "error",
                        "tokens": "0",
                    })
                    await ctx.log("error", f"Worker {wpid} failed: {e}")

                completed[0] += 1
                pct = 25.0 + (55.0 * completed[0] / total_subtasks)
                await ctx.report_progress(
                    f"[{completed[0]}/{total_subtasks}] {subtask[:40]}...", pct,
                )
            return results

        group_results = await asyncio.gather(
            *[run_group(g, wpid)
              for g, wpid in zip(groups, worker_pids)]
        )
        all_results = [r for group_res in group_results for r in group_res]

        await ctx.report_progress("Synthesizing results...", 80.0)

        # --- 4. Synthesize final result ---
        findings_text = "\n\n".join(
            f"### [{r['group']}] {r['subtask']}\n{r['output']}"
            for r in all_results if r["status"] == "ok"
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
        worker_tokens = sum(int(r.get("tokens", "0")) for r in all_results)
        reused_count = sum(1 for g in groups if len(g["subtasks"]) > 1)

        report_data = {
            "task": description,
            "groups": [{"name": g["name"], "subtasks": g["subtasks"]} for g in groups],
            "results": all_results,
            "synthesis": report,
            "total_tokens": self.llm.total_tokens + worker_tokens,
            "orchestrator_tokens": self.llm.total_tokens,
            "worker_tokens": worker_tokens,
            "workers_spawned": len(worker_pids),
            "groups_count": len(groups),
            "subtasks_total": total_subtasks,
            "workers_reused": reused_count,
        }
        report_json = json.dumps(report_data, indent=2, ensure_ascii=False)

        safe_key = description.lower().replace(" ", "-")[:40]
        await ctx.store_artifact(
            key=f"result-{safe_key}",
            content=report_json.encode("utf-8"),
            content_type="application/json",
        )

        # --- 6. Kill workers ---
        async def _kill_safe(wpid):
            try:
                await ctx.kill(wpid)
            except Exception:
                pass  # Already exited

        await asyncio.gather(*[_kill_safe(wpid) for wpid in worker_pids])

        await ctx.log("info", f"Orchestrator done. {len(groups)} groups, "
                       f"{total_subtasks} subtasks, {reused_count} workers reused, "
                       f"{self.llm.total_tokens + worker_tokens} tokens total")

        return TaskResult(
            exit_code=0,
            output=report,
            artifacts={"total_tokens": str(self.llm.total_tokens + worker_tokens)},
            metadata={
                "subtasks_count": str(total_subtasks),
                "groups_count": str(len(groups)),
                "workers_reused": str(reused_count),
            },
        )

    # --- Decomposition ---

    async def _decompose_and_group(self, description, max_workers):
        """Ask LLM to decompose task into grouped subtasks."""
        prompt = (
            f"You are a task planner. Break down the following task into "
            f"subtask groups that can be worked on efficiently.\n\n"
            f"Rules:\n"
            f"- Related subtasks that benefit from shared context: SAME group "
            f"(executed sequentially by one worker)\n"
            f"- Independent subtasks: SEPARATE groups "
            f"(executed in parallel by different workers)\n"
            f"- Maximum {max_workers} groups\n"
            f"- Each group needs at least 1 subtask\n\n"
            f"Task: {description}\n\n"
            f"Return ONLY a JSON object:\n"
            f'{{"groups": ['
            f'{{"name": "short-name", "subtasks": ["subtask description", ...]}}, '
            f"...]}}\n"
            f"No other text."
        )

        raw = await self.ask(prompt, max_tokens=1024, temperature=0.5)
        return self._parse_groups(raw, max_workers)

    def _parse_groups(self, raw: str, max_workers: int) -> list[dict]:
        """Parse LLM response into groups. Multiple fallback strategies."""
        text = raw.strip()
        if text.startswith("```"):
            lines = text.split("\n")
            text = "\n".join(l for l in lines if not l.strip().startswith("```"))
            text = text.strip()

        # Strategy 1: Parse as {"groups": [...]}
        try:
            data = json.loads(text)
            if isinstance(data, dict) and "groups" in data:
                groups = data["groups"]
                if isinstance(groups, list):
                    valid = []
                    for g in groups:
                        if isinstance(g, dict):
                            subtasks = g.get("subtasks", [])
                            if isinstance(subtasks, list) and subtasks:
                                valid.append({
                                    "name": str(g.get("name", f"group-{len(valid)+1}")),
                                    "subtasks": [str(s) for s in subtasks if str(s).strip()],
                                })
                    if valid:
                        return self._cap_groups(valid, max_workers)
        except (json.JSONDecodeError, TypeError, ValueError):
            pass

        # Strategy 2: Parse as flat JSON list -> one subtask per group
        try:
            data = json.loads(text)
            if isinstance(data, list) and data:
                groups = [
                    {"name": f"task-{i+1}", "subtasks": [str(s)]}
                    for i, s in enumerate(data)
                    if str(s).strip()
                ]
                if groups:
                    return self._cap_groups(groups, max_workers)
        except (json.JSONDecodeError, TypeError, ValueError):
            pass

        # Strategy 3: Split by newlines -> one subtask per group
        lines = [
            s.strip().strip('"').strip("- ").strip("0123456789.)")
            for s in raw.strip().split("\n")
            if s.strip() and not s.strip().startswith("```")
        ]
        subtasks = [s for s in lines if s]
        if subtasks:
            groups = [{"name": f"task-{i+1}", "subtasks": [s]}
                      for i, s in enumerate(subtasks)]
            return self._cap_groups(groups, max_workers)

        return []

    @staticmethod
    def _cap_groups(groups: list[dict], max_workers: int) -> list[dict]:
        """Merge excess groups into first N when there are more groups than workers."""
        if len(groups) <= max_workers:
            return groups
        merged = [{"name": g["name"], "subtasks": list(g["subtasks"])}
                  for g in groups[:max_workers]]
        for i, extra in enumerate(groups[max_workers:]):
            target = merged[i % max_workers]
            target["subtasks"].extend(extra["subtasks"])
        return merged
