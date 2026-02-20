"""Dialog logger -- writes full LLM request/response to JSONL."""

import json
import os
from datetime import datetime


class DialogLogger:
    """Appends one JSONL line per LLM call to a shared session log file.

    All agents in the same kernel session write to the same file
    (session timestamp comes from HIVE_SESSION_TS env var).
    """

    def __init__(self, pid: int, agent_name: str):
        session_ts = os.environ.get("HIVE_SESSION_TS", "")
        if not session_ts:
            session_ts = datetime.now().strftime("%Y%m%d-%H%M%S")
        log_dir = os.path.join("logs", "dialogs")
        os.makedirs(log_dir, exist_ok=True)
        self._path = os.path.join(log_dir, f"{session_ts}.jsonl")
        self._pid = pid
        self._agent = agent_name
        self.trace_id: str = ""

    def log(
        self,
        method: str,
        model: str,
        messages: list,
        tools: list | None,
        params: dict,
        response: dict,
        usage: dict,
        latency_ms: float,
        generation_id: str = "",
        provider: str = "",
    ) -> None:
        """Write a single JSONL entry for an LLM call."""
        entry = {
            "ts": datetime.now().isoformat(),
            "pid": self._pid,
            "agent": self._agent,
            "trace_id": self.trace_id,
            "generation_id": generation_id,
            "provider": provider,
            "model": model,
            "method": method,
            "request": {
                "messages": messages,
                "tools": tools,
                **params,
            },
            "response": response,
            "usage": usage,
            "latency_ms": round(latency_ms, 1),
        }
        try:
            with open(self._path, "a", encoding="utf-8") as f:
                f.write(json.dumps(entry, ensure_ascii=False) + "\n")
        except Exception:
            pass  # never crash the agent
