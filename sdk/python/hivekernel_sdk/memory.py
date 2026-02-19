"""AgentMemory -- artifact-backed persistent memory for agents."""

from __future__ import annotations

import json
import logging
from dataclasses import dataclass, field
from typing import Any

logger = logging.getLogger("hivekernel.memory")


class AgentMemory:
    """Persistent memory backed by HiveKernel artifact store.

    Stores:
    - long_term: persistent knowledge text (like MEMORY.md)
    - messages: session conversation history
    - summary: compressed old conversation
    """

    def __init__(self, pid: int) -> None:
        self.pid = pid
        self.long_term: str = ""
        self.messages: list[dict] = []
        self.summary: str = ""

    def _key(self, suffix: str) -> str:
        return f"agent:{self.pid}:{suffix}"

    async def load(self, ctx: Any) -> None:
        """Load memory from artifact store. Tolerant of missing keys."""
        try:
            art = await ctx.get_artifact(self._key("memory"))
            if art and hasattr(art, "content"):
                self.long_term = art.content.decode("utf-8", errors="replace")
            elif isinstance(art, bytes):
                self.long_term = art.decode("utf-8", errors="replace")
        except Exception:
            logger.debug("No long-term memory found for PID %d", self.pid)

        try:
            art = await ctx.get_artifact(self._key("session"))
            if art and hasattr(art, "content"):
                data = art.content.decode("utf-8", errors="replace")
            elif isinstance(art, bytes):
                data = art.decode("utf-8", errors="replace")
            else:
                data = None
            if data:
                self.messages = json.loads(data)
        except Exception:
            logger.debug("No session history found for PID %d", self.pid)

        try:
            art = await ctx.get_artifact(self._key("summary"))
            if art and hasattr(art, "content"):
                self.summary = art.content.decode("utf-8", errors="replace")
            elif isinstance(art, bytes):
                self.summary = art.decode("utf-8", errors="replace")
        except Exception:
            logger.debug("No summary found for PID %d", self.pid)

    async def save(self, ctx: Any) -> None:
        """Persist memory to artifact store."""
        try:
            await ctx.store_artifact(
                self._key("memory"), self.long_term.encode("utf-8")
            )
            await ctx.store_artifact(
                self._key("session"),
                json.dumps(self.messages).encode("utf-8"),
            )
            await ctx.store_artifact(
                self._key("summary"), self.summary.encode("utf-8")
            )
        except Exception as e:
            logger.warning("Failed to save memory for PID %d: %s", self.pid, e)

    def add_message(
        self,
        role: str,
        content: str,
        tool_calls: list[dict] | None = None,
        tool_call_id: str = "",
    ) -> None:
        """Add a message to session history."""
        msg: dict[str, Any] = {"role": role, "content": content}
        if tool_calls:
            msg["tool_calls"] = tool_calls
        if tool_call_id:
            msg["tool_call_id"] = tool_call_id
        self.messages.append(msg)

    def get_context_messages(self, system_prompt: str) -> list[dict]:
        """Build full message array for LLM call.

        Returns: [system, ...session_messages]
        """
        parts = [system_prompt]
        if self.long_term:
            parts.append(f"\n## Long-term Memory\n{self.long_term}")
        if self.summary:
            parts.append(f"\n## Conversation Summary\n{self.summary}")

        result: list[dict] = [{"role": "system", "content": "\n".join(parts)}]
        result.extend(self.messages)
        return result

    def estimate_tokens(self) -> int:
        """Rough token estimate: ~2.5 chars per token."""
        total = sum(len(str(m.get("content", ""))) for m in self.messages)
        return int(total / 2.5)

    def needs_summarization(self, threshold: int = 20, max_tokens: int = 60000) -> bool:
        """Check if messages should be summarized (count or token trigger)."""
        return len(self.messages) > threshold or self.estimate_tokens() > max_tokens

    def force_compress(self) -> None:
        """Emergency compression: drop oldest 50%, keep at least last 2 messages."""
        if len(self.messages) <= 2:
            return
        keep = max(2, len(self.messages) // 2)
        self.messages = self.messages[-keep:]
