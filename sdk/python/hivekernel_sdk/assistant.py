"""AssistantAgent -- 24/7 chat assistant daemon for HiveKernel.

Handles user messages via execute_on. Capabilities:
- Answer questions using LLM
- Schedule cron tasks (via CoreClient.add_cron)
- Delegate coding tasks to Coder agent (via ctx.execute_on)

Dashboard passes sibling PIDs in task params so Assistant can delegate.

Runtime image: hivekernel_sdk.assistant:AssistantAgent
"""

import json
import logging

from .llm_agent import LLMAgent
from .syscall import SyscallContext
from .types import TaskResult

logger = logging.getLogger("hivekernel.assistant")

SYSTEM_PROMPT = (
    "You are HiveKernel Assistant, a helpful AI running inside a process tree OS.\n"
    "You can:\n"
    "- Answer questions on any topic\n"
    "- Schedule recurring tasks using cron expressions\n"
    "- Delegate coding tasks to the Coder agent\n"
    "- Check GitHub repos via the GitMonitor agent\n\n"
    "Respond concisely and helpfully.\n"
    "If the user asks to schedule something, include this JSON block in your response:\n"
    'SCHEDULE: {"name": "task-name", "cron": "*/30 * * * *", "target": "github-monitor", '
    '"description": "What to do", "params": {}}\n'
    "If the user asks to write/generate code, include this block:\n"
    'DELEGATE_CODE: {"request": "what to code", "language": "python"}\n'
    "Otherwise just answer the question directly."
)


def _extract_json_block(text: str, marker: str) -> dict | None:
    """Extract a JSON object after a marker like 'SCHEDULE: {...}'."""
    if marker not in text:
        return None
    try:
        idx = text.index(marker) + len(marker)
        json_start = text.index("{", idx)
        depth = 0
        for i in range(json_start, len(text)):
            if text[i] == "{":
                depth += 1
            elif text[i] == "}":
                depth -= 1
                if depth == 0:
                    return json.loads(text[json_start:i + 1])
    except (ValueError, json.JSONDecodeError):
        pass
    return None


class AssistantAgent(LLMAgent):
    """24/7 chat assistant daemon. Receives tasks via execute_on."""

    async def handle_task(self, task, ctx: SyscallContext) -> TaskResult:
        message = task.params.get("message", task.description)
        history_raw = task.params.get("history", "[]")
        sibling_pids_raw = task.params.get("sibling_pids", "{}")

        try:
            history = json.loads(history_raw) if history_raw else []
        except (json.JSONDecodeError, TypeError):
            history = []

        try:
            sibling_pids = json.loads(sibling_pids_raw) if sibling_pids_raw else {}
        except (json.JSONDecodeError, TypeError):
            sibling_pids = {}

        if not message.strip():
            return TaskResult(exit_code=0, output="Hello! I'm the HiveKernel Assistant. How can I help?")

        await ctx.log("info", f"Assistant received: {message[:80]}")

        # Build messages for LLM.
        messages = [{"role": "system", "content": SYSTEM_PROMPT}]
        for h in history[-10:]:
            if isinstance(h, dict) and "role" in h and "content" in h:
                messages.append({"role": h["role"], "content": h["content"]})
        messages.append({"role": "user", "content": message})

        # Call LLM.
        try:
            response = await self.chat(messages, max_tokens=1024)
        except Exception as e:
            logger.warning("LLM call failed: %s", e)
            return TaskResult(exit_code=1, output=f"LLM error: {e}")

        # Post-process: handle scheduling and code delegation.
        extra = ""
        schedule_result = await self._handle_schedule(response, ctx, sibling_pids)
        if schedule_result:
            extra += "\n\n" + schedule_result

        code_result = await self._handle_code_delegation(response, ctx, sibling_pids)
        if code_result:
            extra += "\n\n" + code_result

        # Strip the raw JSON markers from the user-facing response.
        clean_response = response
        for marker in ("SCHEDULE:", "DELEGATE_CODE:"):
            if marker in clean_response:
                # Remove the marker and JSON block.
                idx = clean_response.index(marker)
                block = _extract_json_block(clean_response, marker)
                if block is not None:
                    # Find the end of the JSON in the original text.
                    json_start = clean_response.index("{", idx)
                    depth = 0
                    for i in range(json_start, len(clean_response)):
                        if clean_response[i] == "{":
                            depth += 1
                        elif clean_response[i] == "}":
                            depth -= 1
                            if depth == 0:
                                clean_response = clean_response[:idx] + clean_response[i + 1:]
                                break

        final = clean_response.strip() + extra
        await ctx.log("info", f"Assistant responded ({len(final)} chars)")
        return TaskResult(exit_code=0, output=final)

    async def _handle_schedule(
        self, response: str, ctx: SyscallContext, sibling_pids: dict
    ) -> str | None:
        """Extract SCHEDULE: JSON from response and create cron entry."""
        info = _extract_json_block(response, "SCHEDULE:")
        if not info:
            return None

        target_name = info.get("target", "")
        target_pid = sibling_pids.get(target_name)
        if not target_pid:
            # Try to find by partial match.
            for name, pid in sibling_pids.items():
                if target_name.lower() in name.lower():
                    target_pid = pid
                    break

        if not target_pid:
            return f"[Could not find agent '{target_name}' to schedule]"

        try:
            cron_id = await self.core.add_cron(
                name=info.get("name", "assistant-scheduled"),
                cron_expression=info.get("cron", "0 * * * *"),
                target_pid=int(target_pid),
                description=info.get("description", "Scheduled task"),
                params=info.get("params") or {},
            )
            return f"[Scheduled: {info.get('name', '?')} | cron: {info.get('cron', '?')} | id: {cron_id}]"
        except Exception as e:
            logger.warning("Failed to create cron entry: %s", e)
            return f"[Failed to schedule: {e}]"

    async def _handle_code_delegation(
        self, response: str, ctx: SyscallContext, sibling_pids: dict
    ) -> str | None:
        """Extract DELEGATE_CODE: JSON and delegate to Coder agent."""
        info = _extract_json_block(response, "DELEGATE_CODE:")
        if not info:
            return None

        coder_pid = sibling_pids.get("coder")
        if not coder_pid:
            return "[Coder agent not available]"

        try:
            result = await ctx.execute_on(
                pid=int(coder_pid),
                description=info.get("request", "Write code"),
                params={
                    "request": info.get("request", ""),
                    "language": info.get("language", "python"),
                },
                timeout_seconds=120,
            )
            return f"[Code from Coder agent:]\n{result.output}"
        except Exception as e:
            logger.warning("Code delegation failed: %s", e)
            return f"[Code delegation failed: {e}]"
