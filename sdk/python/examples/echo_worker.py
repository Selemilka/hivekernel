"""
Echo worker agent: echoes task descriptions back.

Demonstrates the HiveAgent lifecycle and execute_on delegation.

Usage:
    python echo_worker.py --port 50100 --core localhost:50051
    python echo_worker.py --port 50100 --core localhost:50051 --delegate
"""

import argparse
import asyncio

from hivekernel_sdk import HiveAgent, TaskResult, AgentConfig


class EchoWorker(HiveAgent):
    """Simple worker that echoes task descriptions back."""

    async def on_init(self, config: AgentConfig) -> None:
        print(f"EchoWorker initialized: {config.name}")

    async def handle_task(self, task, ctx) -> TaskResult:
        print(f"EchoWorker handling task {task.task_id}: {task.description}")

        # If the task description starts with "delegate:", spawn a child
        # and execute_on it.
        if task.description.startswith("delegate:"):
            subtask_desc = task.description[len("delegate:"):]
            print(f"Spawning sub-worker for: {subtask_desc}")

            child_pid = await ctx.spawn(
                name="sub-echo",
                role="task",
                cognitive_tier="operational",
                model="mini",
            )
            print(f"Spawned sub-worker PID {child_pid}")

            child_result = await ctx.execute_on(
                pid=child_pid,
                description=subtask_desc,
            )
            print(f"Sub-worker result: {child_result.output}")

            await ctx.kill(child_pid)

            return TaskResult(
                exit_code=0,
                output=f"Delegated result: {child_result.output}",
                artifacts=child_result.artifacts,
                metadata={"delegated_to": str(child_pid)},
            )

        return TaskResult(
            exit_code=0,
            output=f"Echo: {task.description}",
            artifacts={"echo": task.description},
            metadata={"worker": "echo", "task_id": task.task_id},
        )

    async def on_message(self, message):
        print(f"EchoWorker got message: {message.type} from PID {message.from_pid}")
        from hivekernel_sdk.types import MessageAck
        return MessageAck(status=MessageAck.ACK_ACCEPTED)


def main():
    parser = argparse.ArgumentParser(description="HiveKernel Echo Worker")
    parser.add_argument("--port", type=int, default=50100, help="Agent gRPC port")
    parser.add_argument("--core", default="localhost:50051", help="Core gRPC address")
    args = parser.parse_args()

    agent = EchoWorker()
    asyncio.run(agent.run(agent_addr=f"[::]:{args.port}", core_addr=args.core))


if __name__ == "__main__":
    main()
