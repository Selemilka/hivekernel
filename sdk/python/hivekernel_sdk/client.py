"""Async gRPC client wrapper for CoreService calls."""

import grpc
import grpc.aio

from . import agent_pb2, core_pb2, core_pb2_grpc, agent_pb2_grpc
from .types import ResourceUsage


class CoreClient:
    """Wraps the gRPC CoreService stub with a clean async Python API."""

    def __init__(self, channel: grpc.aio.Channel, pid: int):
        self._stub = core_pb2_grpc.CoreServiceStub(channel)
        self._pid = pid
        self._metadata = [("x-hivekernel-pid", str(pid))]

    @property
    def pid(self) -> int:
        return self._pid

    @pid.setter
    def pid(self, value: int):
        self._pid = value
        self._metadata = [("x-hivekernel-pid", str(value))]

    # --- Process management ---

    async def spawn_child(
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
        role_enum = _role_to_proto(role)
        cog_enum = _cog_to_proto(cognitive_tier)

        pb_limits = None
        if limits:
            pb_limits = agent_pb2.ResourceLimits(**limits)

        resp = await self._stub.SpawnChild(
            agent_pb2.SpawnRequest(
                name=name,
                role=role_enum,
                cognitive_tier=cog_enum,
                model=model,
                system_prompt=system_prompt,
                tools=tools or [],
                limits=pb_limits,
                initial_task=initial_task,
            ),
            metadata=self._metadata,
        )
        if not resp.success:
            raise RuntimeError(f"spawn failed: {resp.error}")
        return resp.child_pid

    async def kill_child(self, pid: int, recursive: bool = True) -> list[int]:
        """Kill a child agent. Returns list of killed PIDs."""
        resp = await self._stub.KillChild(
            agent_pb2.KillRequest(target_pid=pid, recursive=recursive),
            metadata=self._metadata,
        )
        if not resp.success:
            raise RuntimeError(f"kill failed: {resp.error}")
        return list(resp.killed_pids)

    async def get_process_info(self, pid: int = 0):
        """Get info about a process (0 = self)."""
        resp = await self._stub.GetProcessInfo(
            core_pb2.ProcessInfoRequest(pid=pid),
            metadata=self._metadata,
        )
        return resp

    async def list_children(self, recursive: bool = False):
        """List children of this agent."""
        resp = await self._stub.ListChildren(
            core_pb2.ListChildrenRequest(recursive=recursive),
            metadata=self._metadata,
        )
        return list(resp.children)

    # --- IPC ---

    async def send_message(
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
        resp = await self._stub.SendMessage(
            agent_pb2.SendMessageRequest(
                to_pid=to_pid,
                to_queue=to_queue,
                type=type,
                priority=_priority_to_proto(priority),
                payload=payload,
                requires_ack=requires_ack,
                ttl_seconds=ttl,
            ),
            metadata=self._metadata,
        )
        if not resp.delivered:
            raise RuntimeError(f"send failed: {resp.error}")
        return resp.message_id

    # --- Artifacts ---

    async def store_artifact(
        self,
        key: str,
        content: bytes,
        content_type: str = "application/octet-stream",
        visibility: int = 3,  # global
    ) -> str:
        """Store an artifact in shared memory. Returns artifact ID."""
        resp = await self._stub.StoreArtifact(
            agent_pb2.StoreArtifactRequest(
                key=key,
                content=content,
                content_type=content_type,
                visibility=visibility,
            ),
            metadata=self._metadata,
        )
        if not resp.success:
            raise RuntimeError(f"store artifact failed: {resp.error}")
        return resp.artifact_id

    async def get_artifact(self, key: str = "", artifact_id: str = ""):
        """Get an artifact by key or ID."""
        resp = await self._stub.GetArtifact(
            agent_pb2.GetArtifactRequest(key=key, artifact_id=artifact_id),
            metadata=self._metadata,
        )
        if not resp.found:
            raise RuntimeError(f"artifact not found: {resp.error}")
        return resp

    async def list_artifacts(self, prefix: str = ""):
        """List artifacts with optional prefix filter."""
        resp = await self._stub.ListArtifacts(
            core_pb2.ListArtifactsRequest(prefix=prefix),
            metadata=self._metadata,
        )
        return list(resp.artifacts)

    # --- Resources ---

    async def get_resource_usage(self) -> ResourceUsage:
        """Get resource usage for this agent."""
        resp = await self._stub.GetResourceUsage(
            core_pb2.ResourceUsageRequest(),
            metadata=self._metadata,
        )
        return ResourceUsage(
            tokens_consumed=resp.tokens_consumed,
            tokens_remaining=resp.tokens_remaining,
            children_active=resp.children_active,
            children_max=resp.children_max,
            context_usage_percent=resp.context_usage_percent,
            uptime_seconds=resp.uptime_seconds,
        )

    async def request_resources(self, resource_type: str = "tokens", amount: int = 0) -> dict:
        """Request additional resources from parent's budget."""
        resp = await self._stub.RequestResources(
            core_pb2.ResourceRequest(
                resource_type=resource_type,
                amount=amount,
            ),
            metadata=self._metadata,
        )
        return {
            "granted": resp.granted,
            "granted_amount": resp.granted_amount,
            "reason": resp.reason,
        }

    # --- Escalation ---

    async def escalate(
        self, issue: str, severity: str = "warning", auto_propagate: bool = True
    ) -> str:
        """Escalate a problem to parent."""
        sev_map = {"info": 0, "warning": 1, "error": 2, "critical": 3}
        resp = await self._stub.Escalate(
            agent_pb2.EscalateRequest(
                issue=issue,
                severity=sev_map.get(severity, 1),
                auto_propagate=auto_propagate,
            ),
            metadata=self._metadata,
        )
        return resp.response

    # --- Logging & Metrics ---

    async def log(self, level: str, message: str, **fields):
        """Write a log entry."""
        level_map = {"debug": 0, "info": 1, "warn": 2, "error": 3}
        await self._stub.Log(
            agent_pb2.LogRequest(
                level=level_map.get(level, 1),
                message=message,
                fields=fields,
            ),
            metadata=self._metadata,
        )

    async def report_metric(self, name: str, value: float, **labels):
        """Report a metric value."""
        await self._stub.ReportMetric(
            core_pb2.MetricRequest(
                name=name,
                value=value,
                labels=labels,
            ),
            metadata=self._metadata,
        )


# --- Proto enum converters ---

_ROLE_MAP = {
    "kernel": agent_pb2.ROLE_KERNEL,
    "daemon": agent_pb2.ROLE_DAEMON,
    "agent": agent_pb2.ROLE_AGENT,
    "architect": agent_pb2.ROLE_ARCHITECT,
    "lead": agent_pb2.ROLE_LEAD,
    "worker": agent_pb2.ROLE_WORKER,
    "task": agent_pb2.ROLE_TASK,
}

_COG_MAP = {
    "strategic": agent_pb2.COG_STRATEGIC,
    "tactical": agent_pb2.COG_TACTICAL,
    "operational": agent_pb2.COG_OPERATIONAL,
}

_PRIORITY_MAP = {
    "critical": agent_pb2.PRIORITY_CRITICAL,
    "high": agent_pb2.PRIORITY_HIGH,
    "normal": agent_pb2.PRIORITY_NORMAL,
    "low": agent_pb2.PRIORITY_LOW,
}


def _role_to_proto(role: str) -> int:
    return _ROLE_MAP.get(role, agent_pb2.ROLE_TASK)


def _cog_to_proto(cog: str) -> int:
    return _COG_MAP.get(cog, agent_pb2.COG_OPERATIONAL)


def _priority_to_proto(p: str) -> int:
    return _PRIORITY_MAP.get(p, agent_pb2.PRIORITY_NORMAL)
