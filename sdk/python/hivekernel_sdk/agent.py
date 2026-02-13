"""HiveAgent base class — the main SDK entrypoint for agent authors."""

import argparse
import logging
import sys
from concurrent import futures

import grpc

from . import agent_pb2, agent_pb2_grpc
from .client import CoreClient
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

    # ─── Properties (read-only) ───

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

    # ─── Author implements these ───

    def on_init(self, config: AgentConfig) -> None:
        """Called after initialization. Override to set up state."""
        pass

    def handle_task(self, task: Task) -> TaskResult:
        """Main method. Receives a task, returns a result."""
        raise NotImplementedError("Subclass must implement handle_task")

    def on_message(self, message: Message) -> MessageAck:
        """Handle incoming message from another agent. Override if needed."""
        return MessageAck(status=MessageAck.ACK_ACCEPTED)

    def on_shutdown(self, reason: str) -> bytes | None:
        """Save state before shutdown. Override if needed."""
        return None

    # ─── SDK-provided methods (call CoreService) ───

    def spawn(
        self,
        name: str,
        role: str,
        cognitive_tier: str,
        system_prompt: str = "",
        model: str = "",
        tools: list[str] | None = None,
        initial_task: str = "",
        limits: dict | None = None,
    ) -> int:
        """Spawn a child agent. Returns child PID."""
        return self._core.spawn_child(
            name=name,
            role=role,
            cognitive_tier=cognitive_tier,
            system_prompt=system_prompt,
            model=model,
            tools=tools,
            initial_task=initial_task,
            limits=limits,
        )

    def kill(self, pid: int, recursive: bool = True) -> list[int]:
        """Kill a child agent (and its children if recursive)."""
        return self._core.kill_child(pid, recursive)

    def send(
        self,
        to_pid: int = 0,
        to_queue: str = "",
        type: str = "default",
        payload: bytes = b"",
        priority: str = "normal",
        requires_ack: bool = False,
        ttl: int = 0,
    ) -> str:
        """Send a message. Returns message_id."""
        return self._core.send_message(
            to_pid=to_pid,
            to_queue=to_queue,
            type=type,
            payload=payload,
            priority=priority,
            requires_ack=requires_ack,
            ttl=ttl,
        )

    def escalate(
        self, issue: str, severity: str = "warning", auto_propagate: bool = True
    ) -> str:
        """Escalate a problem to parent."""
        return self._core.escalate(issue, severity, auto_propagate)

    def get_resources(self):
        """Check remaining resources."""
        return self._core.get_resource_usage()

    def log(self, level: str, message: str, **fields):
        """Write a log entry."""
        self._core.log(level, message, **fields)

    def store_artifact(self, key: str, content: bytes,
                       content_type: str = "text/plain", visibility: int = 3) -> str:
        """Store an artifact in shared memory."""
        return self._core.store_artifact(key, content, content_type, visibility)

    def get_artifact(self, key: str):
        """Get an artifact from shared memory."""
        return self._core.get_artifact(key=key)

    def report_metric(self, name: str, value: float, **labels):
        """Report a metric (e.g. tokens_consumed)."""
        self._core.report_metric(name, value, **labels)

    # ─── gRPC AgentService implementation ───

    def _make_servicer(self):
        """Create the gRPC servicer that delegates to this agent."""
        agent = self

        class Servicer(agent_pb2_grpc.AgentServiceServicer):
            def Init(self, request, context):
                agent._pid = request.pid
                agent._ppid = request.ppid
                agent._user = request.user
                agent._role = _role_from_proto(request.role)

                cfg = request.config
                agent._config = AgentConfig(
                    name=cfg.name if cfg else "",
                    system_prompt=cfg.system_prompt if cfg else "",
                    model=cfg.model if cfg else "",
                    metadata=dict(cfg.metadata) if cfg else {},
                )

                try:
                    agent.on_init(agent._config)
                    logger.info(
                        "Agent %s (PID %d) initialized", agent._config.name, agent._pid
                    )
                    return agent_pb2.InitResponse(ready=True)
                except Exception as e:
                    logger.error("Init failed: %s", e)
                    return agent_pb2.InitResponse(ready=False, error=str(e))

            def Shutdown(self, request, context):
                reason = _shutdown_reason(request.reason)
                logger.info("Shutdown requested: %s", reason)
                snapshot = agent.on_shutdown(reason)
                return agent_pb2.ShutdownResponse(
                    state_snapshot=snapshot or b"",
                )

            def Heartbeat(self, request, context):
                return agent_pb2.HeartbeatResponse(
                    state=agent_pb2.STATE_RUNNING,
                )

            def Interrupt(self, request, context):
                logger.info("Interrupt: task=%s reason=%s", request.task_id, request.reason)
                return agent_pb2.InterruptResponse(acknowledged=True)

            def DeliverMessage(self, request, context):
                msg = Message(
                    message_id=request.message_id,
                    from_pid=request.from_pid,
                    from_name=request.from_name,
                    type=request.type,
                    payload=request.payload,
                    timestamp=request.timestamp,
                    requires_ack=request.requires_ack,
                )
                ack = agent.on_message(msg)
                return agent_pb2.MessageAck(
                    message_id=msg.message_id,
                    status=ack.status,
                    reply=ack.reply,
                )

            def Execute(self, request_iterator, context):
                """Handle the bidirectional Execute stream.

                Phase 0: simple request-response — read TaskRequest, call
                handle_task, yield TaskProgress with result.
                """
                for execute_input in request_iterator:
                    if execute_input.HasField("task"):
                        req = execute_input.task
                        task = Task(
                            task_id=req.task_id,
                            description=req.description,
                            params=dict(req.params),
                            timeout_seconds=req.timeout_seconds,
                            context=req.context,
                            parent_task_id=req.parent_task_id,
                        )
                        logger.info("Executing task %s: %s", task.task_id, task.description)

                        try:
                            result = agent.handle_task(task)
                            yield agent_pb2.TaskProgress(
                                task_id=task.task_id,
                                type=agent_pb2.PROGRESS_COMPLETED,
                                message="completed",
                                progress_percent=100.0,
                                result=agent_pb2.TaskResult(
                                    exit_code=result.exit_code,
                                    output=result.output,
                                    artifacts=result.artifacts,
                                    metadata=result.metadata,
                                ),
                            )
                        except Exception as e:
                            logger.error("Task %s failed: %s", task.task_id, e)
                            yield agent_pb2.TaskProgress(
                                task_id=task.task_id,
                                type=agent_pb2.PROGRESS_FAILED,
                                message=str(e),
                                result=agent_pb2.TaskResult(
                                    exit_code=1,
                                    output=str(e),
                                ),
                            )

        return Servicer()

    # ─── Runner ───

    def run(self, agent_addr: str = "", core_addr: str = "localhost:50051"):
        """
        Start the agent: bind gRPC server, connect to core.

        Args:
            agent_addr: Address to listen on (e.g. "[::]:50100" or "unix:///tmp/agent.sock").
                        If empty, auto-assigned.
            core_addr:  Address of the core gRPC server.
        """
        logging.basicConfig(
            level=logging.INFO,
            format="%(asctime)s [%(name)s] %(levelname)s %(message)s",
        )

        # Connect to core.
        core_channel = grpc.insecure_channel(core_addr)
        self._core = CoreClient(core_channel, pid=0)  # PID set after Init

        # Start agent gRPC server.
        server = grpc.server(futures.ThreadPoolExecutor(max_workers=4))
        agent_pb2_grpc.add_AgentServiceServicer_to_server(self._make_servicer(), server)

        if agent_addr:
            server.add_insecure_port(agent_addr)
        else:
            agent_addr = f"[::]:{server.add_insecure_port('[::]:0')}"

        server.start()
        logger.info("Agent server started on %s, core at %s", agent_addr, core_addr)

        try:
            server.wait_for_termination()
        except KeyboardInterrupt:
            logger.info("Shutting down agent...")
            server.stop(grace=5)


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
