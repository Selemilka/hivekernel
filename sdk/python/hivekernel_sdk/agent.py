"""HiveAgent base class — the main SDK entrypoint for agent authors."""

import asyncio
import logging

import grpc
import grpc.aio

from . import agent_pb2, agent_pb2_grpc
from .client import CoreClient
from .syscall import SyscallContext
from .types import (
    AgentConfig,
    Message,
    MessageAck,
    Task,
    TaskResult,
)

logger = logging.getLogger("hivekernel.agent")


class HiveAgent:
    """
    Base class for HiveKernel agents.

    Subclass and implement handle_task(). The SDK handles lifecycle,
    heartbeat, gRPC server/client wiring.
    """

    def __init__(self):
        self._pid: int = 0
        self._ppid: int = 0
        self._user: str = ""
        self._role: str = ""
        self._config: AgentConfig = AgentConfig()
        self._core: CoreClient | None = None

    # --- Properties (read-only) ---

    @property
    def pid(self) -> int:
        return self._pid

    @property
    def ppid(self) -> int:
        return self._ppid

    @property
    def user(self) -> str:
        return self._user

    @property
    def role(self) -> str:
        return self._role

    @property
    def config(self) -> AgentConfig:
        return self._config

    @property
    def core(self) -> CoreClient | None:
        """Direct access to CoreClient for use in on_init/on_shutdown."""
        return self._core

    # --- Author implements these ---

    async def on_init(self, config: AgentConfig) -> None:
        """Called after initialization. Override to set up state."""
        pass

    async def handle_task(self, task: Task, ctx: SyscallContext) -> TaskResult:
        """Main method. Receives a task and syscall context, returns a result."""
        raise NotImplementedError("Subclass must implement handle_task")

    async def on_message(self, message: Message) -> MessageAck:
        """Handle incoming message from another agent. Override if needed."""
        return MessageAck(status=MessageAck.ACK_ACCEPTED)

    async def on_shutdown(self, reason: str) -> bytes | None:
        """Save state before shutdown. Override if needed."""
        return None

    # --- gRPC AgentService implementation ---

    def _make_servicer(self):
        """Create the gRPC servicer that delegates to this agent."""
        agent = self

        class Servicer(agent_pb2_grpc.AgentServiceServicer):
            async def Init(self, request, context):
                agent._pid = request.pid
                agent._ppid = request.ppid
                agent._user = request.user
                agent._role = _role_from_proto(request.role)

                # Update CoreClient PID so subsequent RPCs carry the right metadata.
                if agent._core is not None:
                    agent._core.pid = request.pid

                cfg = request.config
                agent._config = AgentConfig(
                    name=cfg.name if cfg else "",
                    system_prompt=cfg.system_prompt if cfg else "",
                    model=cfg.model if cfg else "",
                    metadata=dict(cfg.metadata) if cfg else {},
                )

                try:
                    await agent.on_init(agent._config)
                    logger.info(
                        "Agent %s (PID %d) initialized", agent._config.name, agent._pid
                    )
                    return agent_pb2.InitResponse(ready=True)
                except Exception as e:
                    logger.error("Init failed: %s", e)
                    return agent_pb2.InitResponse(ready=False, error=str(e))

            async def Shutdown(self, request, context):
                reason = _shutdown_reason(request.reason)
                logger.info("Shutdown requested: %s", reason)
                snapshot = await agent.on_shutdown(reason)
                return agent_pb2.ShutdownResponse(
                    state_snapshot=snapshot or b"",
                )

            async def Heartbeat(self, request, context):
                return agent_pb2.HeartbeatResponse(
                    state=agent_pb2.STATE_RUNNING,
                )

            async def Interrupt(self, request, context):
                logger.info("Interrupt: task=%s reason=%s", request.task_id, request.reason)
                return agent_pb2.InterruptResponse(acknowledged=True)

            async def DeliverMessage(self, request, context):
                msg = Message(
                    message_id=request.message_id,
                    from_pid=request.from_pid,
                    from_name=request.from_name,
                    type=request.type,
                    payload=request.payload,
                    timestamp=request.timestamp,
                    requires_ack=request.requires_ack,
                )
                ack = await agent.on_message(msg)
                return agent_pb2.MessageAck(
                    message_id=msg.message_id,
                    status=ack.status,
                    reply=ack.reply,
                )

            async def Execute(self, request_iterator, context):
                """Handle the bidirectional Execute stream.

                Runs three concurrent tasks:
                  - stream_reader: reads ExecuteInput, dispatches TaskRequest
                    and SyscallResult
                  - stream_writer: drains output queue, writes to stream
                  - run_task: calls handle_task, puts result on queue
                """
                out_queue = asyncio.Queue()
                current_ctx = None
                task_obj = None
                task_received = asyncio.Event()

                async def stream_reader():
                    nonlocal current_ctx, task_obj
                    async for execute_input in request_iterator:
                        if execute_input.HasField("task"):
                            req = execute_input.task
                            task_obj = Task(
                                task_id=req.task_id,
                                description=req.description,
                                params=dict(req.params),
                                timeout_seconds=req.timeout_seconds,
                                context=req.context,
                                parent_task_id=req.parent_task_id,
                            )
                            current_ctx = SyscallContext(task_obj.task_id, out_queue)
                            task_received.set()

                        elif execute_input.HasField("syscall_result"):
                            if current_ctx is not None:
                                current_ctx.resolve_syscall(execute_input.syscall_result)

                async def run_task():
                    await task_received.wait()
                    logger.info("Executing task %s: %s", task_obj.task_id, task_obj.description)
                    try:
                        result = await agent.handle_task(task_obj, current_ctx)
                        await out_queue.put(agent_pb2.TaskProgress(
                            task_id=task_obj.task_id,
                            type=agent_pb2.PROGRESS_COMPLETED,
                            message="completed",
                            progress_percent=100.0,
                            result=agent_pb2.TaskResult(
                                exit_code=result.exit_code,
                                output=result.output,
                                artifacts=result.artifacts,
                                metadata=result.metadata,
                            ),
                        ))
                    except Exception as e:
                        logger.error("Task %s failed: %s", task_obj.task_id, e)
                        await out_queue.put(agent_pb2.TaskProgress(
                            task_id=task_obj.task_id,
                            type=agent_pb2.PROGRESS_FAILED,
                            message=str(e),
                            result=agent_pb2.TaskResult(
                                exit_code=1,
                                output=str(e),
                            ),
                        ))
                    # Signal stream_writer to stop.
                    await out_queue.put(None)

                async def stream_writer():
                    while True:
                        progress = await out_queue.get()
                        if progress is None:
                            return
                        await context.write(progress)

                reader_task = asyncio.create_task(stream_reader())
                writer_task = asyncio.create_task(stream_writer())
                runner_task = asyncio.create_task(run_task())

                # Wait for the task runner to finish (it signals writer via None).
                await runner_task
                await writer_task
                reader_task.cancel()

        return Servicer()

    # --- Runner ---

    async def run(self, agent_addr: str = "", core_addr: str = "localhost:50051"):
        """
        Start the agent: bind async gRPC server, connect to core.

        Args:
            agent_addr: Address to listen on (e.g. "[::]:50100").
                        If empty, auto-assigned.
            core_addr:  Address of the core gRPC server.
        """
        logging.basicConfig(
            level=logging.INFO,
            format="%(asctime)s [%(name)s] %(levelname)s %(message)s",
        )

        # Connect to core.
        core_channel = grpc.aio.insecure_channel(core_addr)
        self._core = CoreClient(core_channel, pid=0)  # PID set after Init

        # Start async agent gRPC server.
        server = grpc.aio.server()
        agent_pb2_grpc.add_AgentServiceServicer_to_server(self._make_servicer(), server)

        if agent_addr:
            server.add_insecure_port(agent_addr)
        else:
            port = server.add_insecure_port("[::]:0")
            agent_addr = f"[::]:{port}"

        await server.start()
        logger.info("Agent server started on %s, core at %s", agent_addr, core_addr)

        try:
            await server.wait_for_termination()
        except asyncio.CancelledError:
            logger.info("Shutting down agent...")
            await server.stop(grace=5)


# --- Helpers ---

_ROLE_NAMES = {
    0: "kernel", 1: "daemon", 2: "agent", 3: "architect",
    4: "lead", 5: "worker", 6: "task",
}


def _role_from_proto(val: int) -> str:
    return _ROLE_NAMES.get(val, "unknown")


def _shutdown_reason(val: int) -> str:
    reasons = {
        0: "normal", 1: "parent_died", 2: "budget_exceeded",
        3: "migration", 4: "user_request",
    }
    return reasons.get(val, "unknown")
