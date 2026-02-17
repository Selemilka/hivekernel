"""AssistantAgent -- 24/7 chat assistant daemon for HiveKernel.

Handles user messages via execute_on. Capabilities:
- Answer questions using LLM
- Schedule cron tasks (via CoreClient.add_cron)
- Delegate coding tasks to Coder agent (via ctx.execute_on)
- Async delegation to sibling agents (fire-and-forget, results via mailbox)

Dashboard passes sibling PIDs in task params so Assistant can delegate.

Runtime image: hivekernel_sdk.assistant:AssistantAgent
"""

import json
import logging
import time
import uuid

from .llm_agent import LLMAgent
from .syscall import SyscallContext
from .types import Message, MessageAck, TaskResult

logger = logging.getLogger("hivekernel.assistant")

DELEGATION_TIMEOUT = 600  # seconds before a delegation is considered stale

SYSTEM_PROMPT = (
    "You are HiveKernel Assistant, a helpful AI running inside a process tree OS.\n"
    "You can:\n"
    "- Answer questions on any topic\n"
    "- Schedule recurring tasks using cron expressions\n"
    "- Delegate tasks to sibling agents (Queen, Coder, etc.) via IPC messaging\n"
    "- Check GitHub repos via the GitMonitor agent\n\n"
    "Delegation is ASYNC: when you delegate to another agent, the task is sent and you "
    "respond immediately. Results arrive with the user's next message.\n\n"
    "Respond concisely and helpfully.\n"
    "If the user asks to schedule something, include this JSON block in your response:\n"
    'SCHEDULE: {"name": "task-name", "cron": "*/30 * * * *", "target": "github-monitor", '
    '"description": "What to do", "params": {}}\n'
    "If the user asks to write/generate code, include this block:\n"
    'DELEGATE_CODE: {"request": "what to code", "language": "python"}\n'
    "If the user asks you to delegate a task to another agent (like Queen for research, "
    "analysis, or complex tasks), include this block:\n"
    'DELEGATE: {"to": "<agent-name>", "task": "description of what to do"}\n'
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

    def __init__(self):
        super().__init__()
        # request_id -> {target, task, ts}
        self._active_delegations: dict[str, dict] = {}

    async def on_message(self, message: Message) -> MessageAck:
        """Override to log and store delegation results when they arrive."""
        # Check if this is a delegation result BEFORE super() processes it.
        is_delegation = (
            message.type == "task_response"
            and message.reply_to
            and message.reply_to in self._active_delegations
        )

        # Let base class handle (stashes in _pending_results or resolves future).
        ack = await super().on_message(message)

        # If it was a delegation result, make it visible immediately.
        if is_delegation and self.core:
            delegation = self._active_delegations.get(message.reply_to)
            if delegation:
                target = delegation["target"]
                try:
                    result_text = message.payload.decode("utf-8", errors="replace")
                    # Parse to get just the output field.
                    try:
                        data = json.loads(result_text)
                        output = data.get("output", result_text)
                    except (json.JSONDecodeError, ValueError):
                        output = result_text
                    # Store full result as artifact.
                    safe_key = delegation["task"].lower().replace(" ", "-")[:40]
                    await self.core.store_artifact(
                        key=f"assistant/delegation-result/{safe_key}",
                        content=message.payload,
                        content_type="application/json",
                    )
                    # Log so it appears in event stream.
                    await self.core.log(
                        "info",
                        f"Delegation result from {target}: {output[:500]}",
                    )
                except Exception as e:
                    logger.warning("Failed to record delegation result: %s", e)

        return ack

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

        # --- Drain async mailbox: collect completed delegation results ---
        prefix_parts = []
        completed_results = self.drain_pending_results()
        for msg in completed_results:
            req_id = msg.reply_to
            delegation = self._active_delegations.pop(req_id, None)
            target_name = delegation["target"] if delegation else "unknown"
            try:
                result_data = json.loads(msg.payload.decode("utf-8"))
                output = result_data.get("output", result_data.get("error", "No output"))
            except (json.JSONDecodeError, UnicodeDecodeError):
                output = msg.payload.decode("utf-8", errors="replace")
            prefix_parts.append(f"[Completed result from {target_name}:]\n{output}")

        # Check for stale delegations (timed out).
        now = time.time()
        stale_ids = [
            rid for rid, d in self._active_delegations.items()
            if now - d["ts"] > DELEGATION_TIMEOUT
        ]
        for rid in stale_ids:
            d = self._active_delegations.pop(rid)
            prefix_parts.append(
                f"[Delegation to {d['target']} timed out after {DELEGATION_TIMEOUT}s "
                f"(task: {d['task'][:80]})]"
            )

        result_prefix = ""
        if prefix_parts:
            result_prefix = "\n\n".join(prefix_parts) + "\n\n---\n\n"

        if not message.strip():
            output = result_prefix + "Hello! I'm the HiveKernel Assistant. How can I help?"
            return TaskResult(exit_code=0, output=output)

        await ctx.log("info", f"Assistant received: {message[:80]}")

        # Discover siblings via list_siblings syscall.
        siblings = []
        sibling_map = {}  # name -> pid
        try:
            siblings = await ctx.list_siblings()
            for s in siblings:
                sibling_map[s["name"]] = s["pid"]
                # Also map short names (e.g. "queen" from "queen@vps1").
                short = s["name"].split("@")[0]
                if short not in sibling_map:
                    sibling_map[short] = s["pid"]
            await ctx.log("info", f"Discovered {len(siblings)} siblings: {[s['name'] for s in siblings]}")
        except Exception as e:
            logger.warning("list_siblings failed: %s", e)

        # Merge discovered siblings with dashboard-provided sibling_pids.
        # Include self so assistant can schedule cron jobs targeting itself.
        merged_pids = {**sibling_pids, **sibling_map}
        self_name = self.config.name
        if self.pid and self_name:
            merged_pids[self_name] = self.pid
            short_self = self_name.split("@")[0]
            if short_self not in merged_pids:
                merged_pids[short_self] = self.pid

        # Build sibling context for LLM.
        sibling_context = ""
        if siblings:
            lines = ["Available sibling agents:"]
            for s in siblings:
                lines.append(f"  - {s['name']} (PID {s['pid']}, role={s['role']}, state={s['state']})")
            sibling_context = "\n".join(lines) + "\n\n"

        # Build messages for LLM.
        system_content = SYSTEM_PROMPT
        if sibling_context:
            system_content += "\n\n" + sibling_context
        messages = [{"role": "system", "content": system_content}]
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

        # Post-process: handle scheduling, code delegation, and general delegation.
        extra = ""
        schedule_result = await self._handle_schedule(response, ctx, merged_pids)
        if schedule_result:
            extra += "\n\n" + schedule_result

        code_result = await self._handle_code_delegation(response, ctx, merged_pids)
        if code_result:
            extra += "\n\n" + code_result

        delegate_result = await self._handle_delegation(response, ctx, merged_pids)
        if delegate_result:
            extra += "\n\n" + delegate_result

        # Strip the raw JSON markers from the user-facing response.
        clean_response = response
        for marker in ("SCHEDULE:", "DELEGATE_CODE:", "DELEGATE:"):
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

        # Build suffix showing in-flight delegations.
        suffix = ""
        if self._active_delegations:
            lines = ["\n\n[In-flight delegations:]"]
            for rid, d in self._active_delegations.items():
                elapsed = int(now - d["ts"])
                lines.append(f"  - {d['target']}: {d['task'][:60]} ({elapsed}s ago)")
            suffix = "\n".join(lines)

        final = result_prefix + clean_response.strip() + extra + suffix
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

    async def _handle_delegation(
        self, response: str, ctx: SyscallContext, sibling_pids: dict
    ) -> str | None:
        """Extract DELEGATE: JSON and send task via async fire-and-forget IPC."""
        info = _extract_json_block(response, "DELEGATE:")
        if not info:
            return None

        target_name = info.get("to", "")
        task_desc = info.get("task", "")
        if not target_name or not task_desc:
            return None

        # Resolve agent name -> PID.
        target_pid = sibling_pids.get(target_name)
        if not target_pid:
            # Try partial match (e.g. "queen" matches "queen@vps1").
            for name, pid in sibling_pids.items():
                if target_name.lower() in name.lower():
                    target_pid = pid
                    break
        if not target_pid:
            return f"[Could not find agent '{target_name}' for delegation]"

        await ctx.log("info", f"Delegating (async) to {target_name} (PID {target_pid}): {task_desc[:80]}")

        try:
            trace_id = "t-" + uuid.uuid4().hex[:8]
            payload = json.dumps({
                "task": task_desc,
                "trace_id": trace_id,
            }, ensure_ascii=False).encode("utf-8")
            request_id = await self.send_fire_and_forget(
                ctx=ctx,
                to_pid=int(target_pid),
                type="task_request",
                payload=payload,
            )
            # Track the delegation for mailbox correlation.
            self._active_delegations[request_id] = {
                "target": target_name,
                "task": task_desc,
                "ts": time.time(),
                "trace_id": trace_id,
            }
            return f"[Delegated to {target_name} -- results will appear in next message]"
        except Exception as e:
            logger.warning("Delegation to %s failed: %s", target_name, e)
            return f"[Delegation to {target_name} failed: {e}]"
