"""
Phase 0 demo agent: a simple echo worker.

Receives a task, echoes it back with some metadata.
Demonstrates the HiveAgent lifecycle end-to-end.

Usage:
    python echo_worker.py --port 50100 --core localhost:50051
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
