"""CoderAgent -- on-demand code generation daemon for HiveKernel.

Accepts coding tasks via execute_on from other agents (Assistant, Lead, etc.).
Generates code using a powerful LLM model, stores result as artifact.

Interface:
    task.params["request"]  -- what to code (required)
    task.params["language"] -- target language (default: python)
    task.params["context"]  -- additional context (optional)

Runtime image: hivekernel_sdk.coder:CoderAgent
"""

import logging
import re

from .llm_agent import LLMAgent
from .syscall import SyscallContext
from .types import TaskResult

logger = logging.getLogger("hivekernel.coder")

SYSTEM_TEMPLATE = (
    "You are a skilled {language} programmer working inside HiveKernel, "
    "a process-tree OS for LLM agents.\n"
    "Write clean, well-structured, production-quality code.\n"
    "Return ONLY the code block with brief inline comments.\n"
    "Do not include explanations outside the code unless explicitly asked.\n"
    "If the request is ambiguous, make reasonable assumptions and note them "
    "in a comment at the top."
)


def _extract_code_block(text: str) -> str:
    """Extract code from markdown fenced block if present, else return as-is."""
    match = re.search(r"```\w*\n(.*?)```", text, re.DOTALL)
    if match:
        return match.group(1).strip()
    return text.strip()


class CoderAgent(LLMAgent):
    """On-demand code generation daemon. Receives tasks via execute_on."""

    async def handle_task(self, task, ctx: SyscallContext) -> TaskResult:
        request = task.params.get("request", task.description)
        language = task.params.get("language", "python")
        context = task.params.get("context", "")

        if not request.strip():
            return TaskResult(exit_code=1, output="Empty code request")

        await ctx.log("info", f"Coder: {language} | {request[:80]}")

        system = SYSTEM_TEMPLATE.format(language=language)
        prompt = f"Task: {request}"
        if context:
            prompt = f"Context:\n{context}\n\n{prompt}"

        try:
            response = await self.llm.complete(prompt, system=system, max_tokens=2048)
        except Exception as e:
            logger.warning("LLM call failed: %s", e)
            return TaskResult(exit_code=1, output=f"Code generation failed: {e}")

        code = _extract_code_block(response)

        # Store code as artifact.
        safe_key = re.sub(r"[^a-z0-9-]", "-", request.lower())[:40].strip("-")
        try:
            await ctx.store_artifact(
                key=f"code/{safe_key}",
                content=code.encode("utf-8"),
                content_type=f"text/{language}",
            )
        except Exception as e:
            logger.warning("Failed to store code artifact: %s", e)

        await ctx.log("info", f"Coder generated {len(code)} chars of {language}")
        return TaskResult(exit_code=0, output=code)
