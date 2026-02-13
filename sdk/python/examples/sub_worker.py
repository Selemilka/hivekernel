"""
Minimal worker agent for auto-spawn by the kernel.

Used as runtime_image="examples.sub_worker:SubWorker" in spawn requests.
The kernel launches: python -m hivekernel_sdk.runner --agent examples.sub_worker:SubWorker
"""

from hivekernel_sdk import HiveAgent, TaskResult, AgentConfig


class SubWorker(HiveAgent):
    """Minimal worker that echoes task descriptions back."""

    async def on_init(self, config: AgentConfig) -> None:
        pass  # Ready immediately.

    async def handle_task(self, task, ctx) -> TaskResult:
        return TaskResult(
            exit_code=0,
            output=f"sub-worker done: {task.description}",
            artifacts={"input": task.description},
            metadata={"worker_pid": str(self.pid)},
        )
