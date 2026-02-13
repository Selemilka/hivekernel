"""
Runner entry point for agent processes spawned by the HiveKernel Go core.

The Go runtime manager launches:
    python -m hivekernel_sdk.runner --agent my_module:MyClass --core localhost:50051

Flow:
    1. Parse --agent as module_path:ClassName
    2. importlib.import_module(module_path) -> getattr(mod, ClassName)
    3. Create agent instance
    4. Start grpc.aio.server() on [::]:0 (random port)
    5. Print "READY <port>" to stdout (signal to Go that gRPC server is bound)
    6. await server.wait_for_termination()
"""

import argparse
import asyncio
import importlib
import logging
import os
import sys

import grpc
import grpc.aio

from . import agent_pb2_grpc
from .client import CoreClient
from .syscall import SyscallContext

logger = logging.getLogger("hivekernel.runner")


def _load_agent_class(spec: str):
    """Load agent class from 'module_path:ClassName' spec."""
    if ":" not in spec:
        raise ValueError(
            f"Invalid agent spec '{spec}': expected 'module_path:ClassName'"
        )
    module_path, class_name = spec.rsplit(":", 1)
    mod = importlib.import_module(module_path)
    cls = getattr(mod, class_name)
    return cls


async def run_agent(agent_spec: str, core_addr: str) -> None:
    """Start the agent process: bind gRPC server, signal READY, wait."""
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(name)s] %(levelname)s %(message)s",
    )

    # Load agent class and create instance.
    cls = _load_agent_class(agent_spec)
    agent = cls()

    # Connect to core.
    core_channel = grpc.aio.insecure_channel(core_addr)
    agent._core = CoreClient(core_channel, pid=0)

    # Start async gRPC server on random port.
    server = grpc.aio.server()
    agent_pb2_grpc.add_AgentServiceServicer_to_server(
        agent._make_servicer(), server
    )
    port = server.add_insecure_port("[::]:0")

    await server.start()
    logger.info("Agent server started on port %d, core at %s", port, core_addr)

    # Signal to Go runtime manager that we are ready.
    # This MUST be flushed immediately so the Go side can read it.
    sys.stdout.write(f"READY {port}\n")
    sys.stdout.flush()

    try:
        await server.wait_for_termination()
    except asyncio.CancelledError:
        logger.info("Shutting down agent...")
        await server.stop(grace=5)
    finally:
        await core_channel.close()


def main():
    parser = argparse.ArgumentParser(description="HiveKernel Agent Runner")
    parser.add_argument(
        "--agent",
        required=True,
        help="Agent class spec: 'module_path:ClassName'",
    )
    parser.add_argument(
        "--core",
        default=os.environ.get("HIVEKERNEL_CORE", "localhost:50051"),
        help="Core gRPC address (default: localhost:50051 or HIVEKERNEL_CORE env)",
    )
    args = parser.parse_args()

    asyncio.run(run_agent(args.agent, args.core))


if __name__ == "__main__":
    main()
