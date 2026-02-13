"""Core types for the HiveKernel Python SDK."""

from dataclasses import dataclass, field


@dataclass
class AgentConfig:
    """Configuration passed to the agent on init."""
    name: str = ""
    system_prompt: str = ""
    model: str = ""
    metadata: dict[str, str] = field(default_factory=dict)


@dataclass
class Task:
    """A task received by an agent."""
    task_id: str = ""
    description: str = ""
    params: dict[str, str] = field(default_factory=dict)
    priority: str = "normal"
    timeout_seconds: int = 0
    context: bytes = b""
    parent_task_id: str = ""


@dataclass
class TaskResult:
    """Result returned from task execution."""
    exit_code: int = 0
    output: str = ""
    artifacts: dict[str, str] = field(default_factory=dict)
    metadata: dict[str, str] = field(default_factory=dict)


@dataclass
class Message:
    """An IPC message from another agent."""
    message_id: str = ""
    from_pid: int = 0
    from_name: str = ""
    relation: str = "system"
    type: str = ""
    priority: str = "normal"
    payload: bytes = b""
    timestamp: int = 0
    requires_ack: bool = False


@dataclass
class MessageAck:
    """Acknowledgement of a received message."""
    ACK_ACCEPTED = 0
    ACK_QUEUED = 1
    ACK_REJECTED = 2
    ACK_ESCALATE = 3

    message_id: str = ""
    status: int = 0  # ACK_ACCEPTED
    reply: str = ""


@dataclass
class ResourceUsage:
    """Current resource usage for this agent."""
    tokens_consumed: int = 0
    tokens_remaining: int = 0
    children_active: int = 0
    children_max: int = 0
    context_usage_percent: float = 0.0
    uptime_seconds: int = 0
