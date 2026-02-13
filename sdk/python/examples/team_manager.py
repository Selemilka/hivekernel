"""
Team manager agent: spawns workers, delegates tasks, collects results.

Used as runtime_image="examples.team_manager:TeamManager" in spawn requests.
The kernel launches: python -m hivekernel_sdk.runner --agent examples.team_manager:TeamManager
"""

import json

from hivekernel_sdk import HiveAgent, TaskResult, AgentConfig
from hivekernel_sdk.syscall import SyscallContext


class TeamManager(HiveAgent):
    """Manager that spawns sub-workers and delegates tasks to them."""

    async def on_init(self, config: AgentConfig) -> None:
        pass

    async def handle_task(self, task, ctx: SyscallContext) -> TaskResult:
        """
        Task params:
          - worker_count: how many workers to spawn (default 2)
          - subtasks: JSON array of task descriptions (default: auto-generated)
        """
        worker_count = int(task.params.get("worker_count", "2"))
        subtasks_raw = task.params.get("subtasks", "")
        if subtasks_raw:
            subtasks = json.loads(subtasks_raw)
        else:
            subtasks = [f"subtask-{i}" for i in range(worker_count)]

        await ctx.log("info", f"Manager starting: {worker_count} workers, {len(subtasks)} subtasks")
        await ctx.report_progress("spawning workers", 10.0)

        # 1. Spawn workers.
        worker_pids = []
        for i in range(worker_count):
            pid = await ctx.spawn(
                name=f"worker-{i}",
                role="task",
                cognitive_tier="operational",
                model="mini",
                runtime_image="examples.sub_worker:SubWorker",
                runtime_type="python",
            )
            worker_pids.append(pid)
            await ctx.log("info", f"Spawned worker-{i} with PID {pid}")

        await ctx.report_progress("workers spawned", 30.0)

        # 2. Delegate subtasks round-robin.
        results = []
        for i, subtask_desc in enumerate(subtasks):
            target_pid = worker_pids[i % len(worker_pids)]
            await ctx.report_progress(
                f"executing subtask {i+1}/{len(subtasks)}",
                30.0 + (60.0 * (i + 1) / len(subtasks)),
            )
            result = await ctx.execute_on(
                pid=target_pid,
                description=subtask_desc,
            )
            results.append({
                "subtask": subtask_desc,
                "worker_pid": target_pid,
                "exit_code": result.exit_code,
                "output": result.output,
            })

        await ctx.report_progress("subtasks complete", 90.0)

        # 3. Store combined result as artifact.
        summary = json.dumps(results, indent=2)
        artifact_id = await ctx.store_artifact(
            key=f"team-result-{task.task_id}",
            content=summary.encode("utf-8"),
            content_type="application/json",
        )

        # 4. Kill workers.
        for pid in worker_pids:
            await ctx.kill(pid)

        await ctx.log("info", f"Manager done: {len(results)} subtasks, artifact={artifact_id}")

        return TaskResult(
            exit_code=0,
            output=f"Completed {len(results)} subtasks with {worker_count} workers",
            artifacts={"summary": summary, "artifact_id": artifact_id},
            metadata={
                "worker_count": str(worker_count),
                "subtask_count": str(len(subtasks)),
            },
        )
