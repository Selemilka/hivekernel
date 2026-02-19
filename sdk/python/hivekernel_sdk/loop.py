"""AgentLoop -- iterative LLM + tool execution engine."""

from __future__ import annotations

import json
import logging
import time
from dataclasses import dataclass, field
from typing import Any

from .llm import LLMClient
from .memory import AgentMemory
from .tools import ToolContext, ToolRegistry

logger = logging.getLogger("hivekernel.loop")


@dataclass
class LoopResult:
    """Result of an agent loop run."""

    content: str
    iterations: int
    tool_calls_total: int
    prompt_tokens: int = 0
    completion_tokens: int = 0
    total_tokens: int = 0
    llm_calls: int = 0
    total_latency_ms: float = 0.0


class AgentLoop:
    """Iterative LLM + tool execution engine.

    Modeled on PicoClaw's runLLMIteration: calls the LLM, executes any
    tool calls, feeds results back, and repeats until the LLM returns
    a final text response or max_iterations is reached.
    """

    def __init__(
        self,
        llm: LLMClient,
        registry: ToolRegistry,
        memory: AgentMemory,
        max_iterations: int = 15,
    ) -> None:
        self.llm = llm
        self.registry = registry
        self.memory = memory
        self.max_iterations = max_iterations

    async def run(
        self,
        prompt: str,
        ctx: ToolContext,
        system_prompt: str = "",
        model: str = "",
    ) -> LoopResult:
        """Run the agent loop.

        1. Add user message to memory
        2. Build messages from memory (system + history)
        3. Call llm.chat_with_tools(messages, tools)
        4. If no tool_calls: save memory, return content
        5. Record assistant message with tool_calls
        6. For each tool_call: execute via registry, record tool result
        7. Go to step 2
        8. After loop: summarize if needed, save memory
        """
        self.memory.add_message("user", prompt)

        # Snapshot LLM metrics before loop to compute deltas.
        llm_start_calls = self.llm.llm_calls
        llm_start_prompt = self.llm.prompt_tokens
        llm_start_completion = self.llm.completion_tokens
        llm_start_total = self.llm.total_tokens
        llm_start_latency = self.llm.total_latency_ms

        tools_schema = self.registry.to_openai_schema() or None
        iterations = 0
        tool_calls_total = 0
        final_content = ""

        for _ in range(self.max_iterations):
            iterations += 1

            messages = self.memory.get_context_messages(system_prompt)

            # Retry with compression on context overflow.
            choice = None
            for retry in range(3):
                try:
                    choice = await self.llm.chat_with_tools(
                        messages=messages[1:],  # skip system, pass as param
                        tools=tools_schema,
                        system=messages[0]["content"] if messages else "",
                        model=model,
                    )
                    break
                except RuntimeError as e:
                    err_msg = str(e).lower()
                    if "context" in err_msg or "token" in err_msg or "length" in err_msg:
                        logger.warning("Context overflow, compressing (retry %d)", retry + 1)
                        self.memory.force_compress()
                        messages = self.memory.get_context_messages(system_prompt)
                        continue
                    raise
            if choice is None:
                raise RuntimeError("Context overflow after 3 retries")

            message = choice["message"]
            finish_reason = choice["finish_reason"]
            content = message.get("content", "") or ""

            # Emit llm_call event with prompt/response previews.
            lc = self.llm.last_call
            try:
                # Extract last user message as prompt preview.
                prompt_preview = ""
                for m in reversed(messages):
                    if m.get("role") == "user":
                        prompt_preview = (m.get("content", "") or "")[:500]
                        break
                response_preview = content[:500]

                await ctx.log_event(
                    "llm_call",
                    f"LLM call: {lc.get('model', '')}",
                    model=lc.get("model", ""),
                    prompt_tokens=str(lc.get("prompt_tokens", 0)),
                    completion_tokens=str(lc.get("completion_tokens", 0)),
                    total_tokens=str(lc.get("total_tokens", 0)),
                    latency_ms=str(lc.get("latency_ms", 0)),
                    iteration=str(iterations),
                    prompt_preview=prompt_preview,
                    response_preview=response_preview,
                )
            except Exception:
                pass  # Best-effort event emission
            tool_calls = message.get("tool_calls")

            if not tool_calls:
                # Final response -- no more tool calls
                self.memory.add_message("assistant", content)
                final_content = content
                break

            # Record assistant message with tool calls
            self.memory.add_message(
                "assistant", content, tool_calls=tool_calls
            )

            # Execute each tool call
            for tc in tool_calls:
                tc_id = tc.get("id", "")
                func = tc.get("function", {})
                tc_name = func.get("name", "")
                tc_args_raw = func.get("arguments", "{}")

                try:
                    tc_args = json.loads(tc_args_raw)
                except (json.JSONDecodeError, TypeError):
                    tc_args = {}

                logger.info("Tool call: %s(%s)", tc_name, tc_args)
                t0 = time.monotonic()
                result = await self.registry.execute(tc_name, ctx, tc_args)
                duration_ms = round((time.monotonic() - t0) * 1000, 1)
                tool_calls_total += 1

                # Emit tool_call event.
                try:
                    await ctx.log_event(
                        "tool_call",
                        f"Tool: {tc_name}",
                        tool_name=tc_name,
                        args_preview=str(tc_args)[:200],
                        result_preview=result.content[:200],
                        duration_ms=str(duration_ms),
                        is_error=str(result.is_error),
                    )
                except Exception:
                    pass  # Best-effort event emission

                self.memory.add_message(
                    "tool", result.content, tool_call_id=tc_id
                )
        else:
            # Max iterations reached
            final_content = content if content else "Max iterations reached"
            logger.warning(
                "Agent loop hit max iterations (%d)", self.max_iterations
            )

        # Summarize if needed
        if self.memory.needs_summarization():
            await self._summarize(system_prompt, model)

        # Save memory
        await self.memory.save(ctx)

        return LoopResult(
            content=final_content,
            iterations=iterations,
            tool_calls_total=tool_calls_total,
            prompt_tokens=self.llm.prompt_tokens - llm_start_prompt,
            completion_tokens=self.llm.completion_tokens - llm_start_completion,
            total_tokens=self.llm.total_tokens - llm_start_total,
            llm_calls=self.llm.llm_calls - llm_start_calls,
            total_latency_ms=round(self.llm.total_latency_ms - llm_start_latency, 1),
        )

    async def _summarize(self, system_prompt: str, model: str) -> None:
        """Compress older messages, keep last 4."""
        if len(self.memory.messages) <= 4:
            return

        old_messages = self.memory.messages[:-4]
        keep_messages = self.memory.messages[-4:]

        summary_prompt = (
            "Summarize this conversation concisely, preserving key facts, "
            "decisions, and results:\n\n"
        )
        for msg in old_messages:
            role = msg.get("role", "?")
            content = msg.get("content", "")
            if content:
                summary_prompt += f"{role}: {content}\n"

        try:
            summary = await self.llm.chat(
                [{"role": "user", "content": summary_prompt}],
                system=system_prompt,
                model=model,
                max_tokens=1024,
            )
            existing = self.memory.summary
            if existing:
                self.memory.summary = f"{existing}\n\n{summary}"
            else:
                self.memory.summary = summary
        except Exception as e:
            logger.warning("Summarization failed: %s", e)

        self.memory.messages = keep_messages
