"""Unit tests for CoderAgent.

Run: python sdk/python/tests/test_coder.py -v
"""

import asyncio
import os
import sys
import unittest
from unittest.mock import AsyncMock, MagicMock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from hivekernel_sdk.coder import CoderAgent, _extract_code_block


# --- Helper factories ---

def _mock_ctx():
    """Create a mock SyscallContext."""
    ctx = AsyncMock()
    ctx.log = AsyncMock()
    ctx.store_artifact = AsyncMock()
    return ctx


def _mock_task(request="Write hello world", language="python", context=""):
    """Create a mock task for Coder."""
    task = MagicMock()
    task.description = request
    task.params = {"request": request, "language": language}
    if context:
        task.params["context"] = context
    return task


class TestExtractCodeBlock(unittest.TestCase):
    """Tests for the _extract_code_block helper."""

    def test_fenced_python(self):
        text = "```python\ndef hello():\n    print('hi')\n```"
        result = _extract_code_block(text)
        self.assertIn("def hello():", result)
        self.assertNotIn("```", result)

    def test_fenced_no_lang(self):
        text = "```\ncode here\n```"
        result = _extract_code_block(text)
        self.assertEqual(result, "code here")

    def test_no_fence(self):
        text = "def hello():\n    pass"
        result = _extract_code_block(text)
        self.assertEqual(result, "def hello():\n    pass")

    def test_text_before_and_after(self):
        text = "Here is the code:\n```python\nprint('x')\n```\nDone."
        result = _extract_code_block(text)
        self.assertEqual(result, "print('x')")

    def test_multiple_blocks_returns_first(self):
        text = "```python\nfirst\n```\n```python\nsecond\n```"
        result = _extract_code_block(text)
        self.assertEqual(result, "first")


class TestCoderAgent(unittest.TestCase):
    """Tests for CoderAgent.handle_task()."""

    def setUp(self):
        self.agent = CoderAgent()
        self.agent.llm = MagicMock()
        self.agent._core = MagicMock()
        self.agent._system_prompt = ""

    def _run(self, coro):
        return asyncio.get_event_loop().run_until_complete(coro)

    def test_basic_code_generation(self):
        """Basic request returns generated code."""
        self.agent.llm.complete = AsyncMock(
            return_value="```python\ndef hello():\n    print('Hello!')\n```"
        )

        task = _mock_task(request="Write hello world function")
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.exit_code, 0)
        self.assertIn("def hello():", result.output)
        self.assertNotIn("```", result.output)

    def test_code_without_fence(self):
        """Response without fenced block is returned as-is."""
        self.agent.llm.complete = AsyncMock(
            return_value="def add(a, b):\n    return a + b"
        )

        task = _mock_task(request="add function")
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.exit_code, 0)
        self.assertIn("return a + b", result.output)

    def test_language_param(self):
        """Language parameter is passed to system prompt."""
        self.agent.llm.complete = AsyncMock(return_value="console.log('hi');")

        task = _mock_task(request="hello world", language="javascript")
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.exit_code, 0)
        # Verify system prompt includes language.
        call_kwargs = self.agent.llm.complete.call_args.kwargs
        self.assertIn("javascript", call_kwargs.get("system", ""))

    def test_context_param(self):
        """Context is prepended to the prompt."""
        self.agent.llm.complete = AsyncMock(return_value="result code")

        task = _mock_task(
            request="Add error handling",
            context="Existing function:\ndef fetch(): pass"
        )
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.exit_code, 0)
        # Verify prompt includes context.
        call_args = self.agent.llm.complete.call_args
        prompt = call_args[0][0]
        self.assertIn("Existing function", prompt)
        self.assertIn("Add error handling", prompt)

    def test_empty_request(self):
        """Empty request returns error."""
        task = _mock_task(request="")
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.exit_code, 1)
        self.assertIn("Empty", result.output)

    def test_llm_error(self):
        """LLM failure returns error TaskResult."""
        self.agent.llm.complete = AsyncMock(side_effect=RuntimeError("API down"))

        task = _mock_task(request="write code")
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.exit_code, 1)
        self.assertIn("failed", result.output.lower())

    def test_stores_artifact(self):
        """Generated code is stored as artifact."""
        self.agent.llm.complete = AsyncMock(return_value="code output")

        task = _mock_task(request="fibonacci function")
        ctx = _mock_ctx()
        self._run(self.agent.handle_task(task, ctx))

        ctx.store_artifact.assert_called_once()
        call_kwargs = ctx.store_artifact.call_args.kwargs
        self.assertTrue(call_kwargs["key"].startswith("code/"))
        self.assertEqual(call_kwargs["content_type"], "text/python")
        self.assertEqual(call_kwargs["content"], b"code output")

    def test_artifact_key_sanitized(self):
        """Artifact key is sanitized from request text."""
        self.agent.llm.complete = AsyncMock(return_value="ok")

        task = _mock_task(request="Write a Quick Sort Algorithm!")
        ctx = _mock_ctx()
        self._run(self.agent.handle_task(task, ctx))

        call_kwargs = ctx.store_artifact.call_args.kwargs
        key = call_kwargs["key"]
        # Should not contain spaces or special chars.
        self.assertNotIn(" ", key)
        self.assertNotIn("!", key)
        self.assertTrue(key.startswith("code/"))

    def test_default_language_python(self):
        """Default language is python when not specified."""
        self.agent.llm.complete = AsyncMock(return_value="pass")

        task = MagicMock()
        task.description = "hello"
        task.params = {"request": "hello"}  # no language param
        ctx = _mock_ctx()
        self._run(self.agent.handle_task(task, ctx))

        call_kwargs = self.agent.llm.complete.call_args.kwargs
        self.assertIn("python", call_kwargs.get("system", ""))

    def test_artifact_store_failure_non_fatal(self):
        """Artifact storage failure does not break the response."""
        self.agent.llm.complete = AsyncMock(return_value="good code")

        task = _mock_task(request="test code")
        ctx = _mock_ctx()
        ctx.store_artifact = AsyncMock(side_effect=RuntimeError("storage error"))
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.exit_code, 0)
        self.assertIn("good code", result.output)


if __name__ == "__main__":
    unittest.main()
