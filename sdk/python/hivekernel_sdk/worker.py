"""Universal LLM Worker agent for HiveKernel.

Receives a task description, executes it via LLM, returns the result.
Used by OrchestratorAgent to handle subtasks.

Phase 4: Worker reuse -- workers maintain accumulated context across
sequential tasks when spawned with role=worker (stays alive between
execute_on calls). Previous task results are included as context for
related follow-up tasks within the same group.

Runtime image: hivekernel_sdk.worker:WorkerAgent
"""

from .llm_agent import LLMAgent
from .syscall import SyscallContext
from .types import TaskResult


class WorkerAgent(LLMAgent):
    """Generic LLM worker: takes a task, asks LLM, returns result.

    Maintains accumulated context across sequential tasks when used
    with role=worker (stays alive between execute_on calls).
    """

    def __init__(self):
        super().__init__()
        self._task_memory: list[dict] = []

    async def handle_task(self, task, ctx: SyscallContext) -> TaskResult:
        description = task.params.get("subtask", task.description)

        await ctx.log("info", f"Worker PID {self.pid} starting: {description[:80]}")
        await ctx.report_progress("Working...", 10.0)

        # Build context from previous tasks in this session.
        context = ""
        if self._task_memory:
            lines = ["You have already completed these related tasks:"]
            for i, m in enumerate(self._task_memory, 1):
                lines.append(f"\n--- Previous task {i} ---")
                lines.append(f"Task: {m['task']}")
                lines.append(f"Result: {m['output'][:500]}")
            lines.append("\n--- Current task ---")
            lines.append("Use context from your previous tasks to inform this response.\n")
            context = "\n".join(lines)

        # Ask LLM to execute the subtask.
        result = await self.ask(
            f"{context}"
            f"Complete the following task thoroughly and concisely.\n\n"
            f"Task: {description}\n\n"
            f"Provide a clear, well-structured response.",
            max_tokens=2048,
            temperature=0.7,
        )

        # Remember for future context within this session.
        self._task_memory.append({"task": description, "output": result})

        tokens = self.llm.total_tokens
        await ctx.log("info", f"Worker PID {self.pid} done ({len(self._task_memory)} tasks, "
                       f"{tokens} tokens)")
        await ctx.report_progress("Done", 100.0)

        return TaskResult(
            exit_code=0,
            output=result,
            metadata={
                "tokens_used": str(tokens),
                "tasks_completed": str(len(self._task_memory)),
            },
        )
