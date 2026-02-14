"""Universal LLM Worker agent for HiveKernel.

Receives a task description, executes it via LLM, returns the result.
Used by OrchestratorAgent to handle subtasks.

Runtime image: hivekernel_sdk.worker:WorkerAgent
"""

from .llm_agent import LLMAgent
from .syscall import SyscallContext
from .types import TaskResult


class WorkerAgent(LLMAgent):
    """Generic LLM worker: takes a task description, asks LLM, returns result."""

    async def handle_task(self, task, ctx: SyscallContext) -> TaskResult:
        description = task.params.get("subtask", task.description)

        await ctx.log("info", f"Worker PID {self.pid} starting: {description[:80]}")
        await ctx.report_progress("Working...", 10.0)

        # Ask LLM to execute the subtask.
        result = await self.ask(
            f"Complete the following task thoroughly and concisely.\n\n"
            f"Task: {description}\n\n"
            f"Provide a clear, well-structured response.",
            max_tokens=2048,
            temperature=0.7,
        )

        tokens = self.llm.total_tokens
        await ctx.log("info", f"Worker PID {self.pid} done, {tokens} tokens")
        await ctx.report_progress("Done", 100.0)

        return TaskResult(
            exit_code=0,
            output=result,
            metadata={"tokens_used": str(tokens)},
        )
