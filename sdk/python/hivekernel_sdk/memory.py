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
        """Rough token estimate for messages only: ~2.5 chars per token."""
        total = sum(len(str(m.get("content", ""))) for m in self.messages)
        return int(total / 2.5)

    def estimate_total_tokens(self) -> int:
        """Rough token estimate including summary and long_term."""
        total = len(self.long_term) + len(self.summary)
        total += sum(
            len(str(m.get("content", "")))
            + len(str(m.get("tool_calls", ""))) * 2
            for m in self.messages
        )
        return int(total / 2.5)

    def needs_summarization(self, threshold: int = 10, max_tokens: int = 30000) -> bool:
        """Check if messages should be summarized (count or token trigger)."""
        return len(self.messages) > threshold or self.estimate_total_tokens() > max_tokens

    def force_compress(self) -> None:
        """Progressive emergency compression.

        Called repeatedly on context overflow. Each call is more aggressive:
        1st call: drop to last 4 messages, truncate summary to 2000 chars
        2nd call: drop to last 2 messages, clear summary
        3rd call: drop to last 1 message, clear summary + truncate long_term
        """
        total_est = self.estimate_total_tokens()
        msg_count = len(self.messages)

        logger.warning(
            "force_compress: %d messages, ~%d tokens, summary=%d chars, long_term=%d chars",
            msg_count, total_est, len(self.summary), len(self.long_term),
        )

        if msg_count > 4:
            # Phase 1: keep last 4 messages, truncate summary.
            self.messages = self.messages[-4:]
            if len(self.summary) > 2000:
                self.summary = self.summary[-2000:]
            return

        if msg_count > 2:
            # Phase 2: keep last 2 messages, clear summary.
            self.messages = self.messages[-2:]
            self.summary = ""
            return

        # Phase 3: keep last message, clear summary, truncate long_term.
        if msg_count > 1:
            self.messages = self.messages[-1:]
        self.summary = ""
        if len(self.long_term) > 1000:
            self.long_term = self.long_term[-1000:]
