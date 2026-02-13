"""SyscallContext — bridges handle_task() to the Execute bidirectional stream."""

import asyncio
import uuid

from . import agent_pb2
from .client import _role_to_proto, _cog_to_proto, _priority_to_proto


class SyscallContext:
    """
    Passed to handle_task(task, ctx) so the agent can perform syscalls
    (spawn, kill, send, etc.) through the Execute bidi stream without
    breaking execution flow.

    Each syscall:
      1. Creates a SystemCall protobuf with a unique call_id
      2. Puts a TaskProgress(type=PROGRESS_SYSCALL) on the output queue
      3. Awaits an asyncio.Future that resolves when the matching
         SyscallResult arrives from the core
    """

    def __init__(self, task_id: str, out_queue: asyncio.Queue):
        self._task_id = task_id
        self._out_queue = out_queue
        self._pending: dict[str, asyncio.Future] = {}

    @property
    def task_id(self) -> str:
        return self._task_id

    def resolve_syscall(self, result: agent_pb2.SyscallResult) -> None:
        """Called by the stream reader when a SyscallResult arrives."""
        fut = self._pending.pop(result.call_id, None)
        if fut and not fut.done():
            fut.set_result(result)

    async def _do_syscall(self, call: agent_pb2.SystemCall) -> agent_pb2.SyscallResult:
        """Send a syscall through the stream and wait for its result."""
        loop = asyncio.get_running_loop()
        fut = loop.create_future()
        self._pending[call.call_id] = fut

        progress = agent_pb2.TaskProgress(
            task_id=self._task_id,
            type=agent_pb2.PROGRESS_SYSCALL,
            message=f"syscall:{call.call_id}",
            syscalls=[call],
        )
        await self._out_queue.put(progress)
        return await fut

    # --- Syscall methods ---

    async def spawn(
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
        pb_limits = None
        if limits:
            pb_limits = agent_pb2.ResourceLimits(**limits)

        call = agent_pb2.SystemCall(
            call_id=str(uuid.uuid4()),
            spawn=agent_pb2.SpawnRequest(
                name=name,
                role=_role_to_proto(role),
                cognitive_tier=_cog_to_proto(cognitive_tier),
                model=model,
                system_prompt=system_prompt,
                tools=tools or [],
                limits=pb_limits,
                initial_task=initial_task,
            ),
        )
        result = await self._do_syscall(call)
        resp = result.spawn
        if not resp.success:
            raise RuntimeError(f"spawn failed: {resp.error}")
        return resp.child_pid

    async def kill(self, pid: int, recursive: bool = True) -> list[int]:
        """Kill a child agent."""
        call = agent_pb2.SystemCall(
            call_id=str(uuid.uuid4()),
            kill=agent_pb2.KillRequest(target_pid=pid, recursive=recursive),
        )
        result = await self._do_syscall(call)
        resp = result.kill
        if not resp.success:
            raise RuntimeError(f"kill failed: {resp.error}")
        return list(resp.killed_pids)

    async def send(
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
        call = agent_pb2.SystemCall(
            call_id=str(uuid.uuid4()),
            send=agent_pb2.SendMessageRequest(
                to_pid=to_pid,
                to_queue=to_queue,
                type=type,
                priority=_priority_to_proto(priority),
                payload=payload,
                requires_ack=requires_ack,
                ttl_seconds=ttl,
            ),
        )
        result = await self._do_syscall(call)
        resp = result.send
        if not resp.delivered:
            raise RuntimeError(f"send failed: {resp.error}")
        return resp.message_id

    async def store_artifact(
        self,
        key: str,
        content: bytes,
        content_type: str = "text/plain",
        visibility: int = 3,
    ) -> str:
        """Store an artifact in shared memory. Returns artifact ID."""
        call = agent_pb2.SystemCall(
            call_id=str(uuid.uuid4()),
            store=agent_pb2.StoreArtifactRequest(
                key=key,
                content=content,
                content_type=content_type,
                visibility=visibility,
            ),
        )
        result = await self._do_syscall(call)
        resp = result.store
        if not resp.success:
            raise RuntimeError(f"store artifact failed: {resp.error}")
        return resp.artifact_id

    async def get_artifact(self, key: str = "", artifact_id: str = ""):
        """Get an artifact by key or ID."""
        call = agent_pb2.SystemCall(
            call_id=str(uuid.uuid4()),
            get_artifact=agent_pb2.GetArtifactRequest(
                key=key, artifact_id=artifact_id,
            ),
        )
        result = await self._do_syscall(call)
        resp = result.get_artifact
        if not resp.found:
            raise RuntimeError(f"artifact not found: {resp.error}")
        return resp

    async def escalate(
        self, issue: str, severity: str = "warning", auto_propagate: bool = True
    ) -> str:
        """Escalate a problem to parent."""
        sev_map = {"info": 0, "warning": 1, "error": 2, "critical": 3}
        call = agent_pb2.SystemCall(
            call_id=str(uuid.uuid4()),
            escalate=agent_pb2.EscalateRequest(
                issue=issue,
                severity=sev_map.get(severity, 1),
                auto_propagate=auto_propagate,
            ),
        )
        result = await self._do_syscall(call)
        return result.escalate.response

    async def log(self, level: str, message: str, **fields):
        """Write a log entry via syscall."""
        level_map = {"debug": 0, "info": 1, "warn": 2, "error": 3}
        call = agent_pb2.SystemCall(
            call_id=str(uuid.uuid4()),
            log=agent_pb2.LogRequest(
                level=level_map.get(level, 1),
                message=message,
                fields=fields,
            ),
        )
        await self._do_syscall(call)

    async def report_progress(self, message: str, percent: float = 0.0):
        """Send a progress update through the stream (not a syscall)."""
        progress = agent_pb2.TaskProgress(
            task_id=self._task_id,
            type=agent_pb2.PROGRESS_UPDATE,
            message=message,
            progress_percent=percent,
        )
        await self._out_queue.put(progress)
