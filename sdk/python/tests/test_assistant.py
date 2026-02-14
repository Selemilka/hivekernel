"""Unit tests for AssistantAgent.

Run: python sdk/python/tests/test_assistant.py -v
"""

import asyncio
import json
import os
import sys
import unittest
from unittest.mock import AsyncMock, MagicMock, patch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from hivekernel_sdk.assistant import AssistantAgent, _extract_json_block


# --- Helper factories ---

def _mock_ctx():
    """Create a mock SyscallContext."""
    ctx = AsyncMock()
    ctx.log = AsyncMock()
    ctx.execute_on = AsyncMock(return_value=MagicMock(
        exit_code=0,
        output="def fibonacci(n):\n    pass",
        artifacts={},
        metadata={},
    ))
    return ctx


def _mock_task(message="Hello", history=None, sibling_pids=None):
    """Create a mock task for Assistant."""
    task = MagicMock()
    task.description = message
    task.params = {
        "message": message,
        "history": json.dumps(history or []),
        "sibling_pids": json.dumps(sibling_pids or {}),
    }
    return task


class TestExtractJsonBlock(unittest.TestCase):
    """Tests for the _extract_json_block helper."""

    def test_extract_schedule(self):
        text = 'Sure! SCHEDULE: {"name": "test", "cron": "*/5 * * * *", "target": "monitor"}'
        result = _extract_json_block(text, "SCHEDULE:")
        self.assertIsNotNone(result)
        self.assertEqual(result["name"], "test")
        self.assertEqual(result["cron"], "*/5 * * * *")
        self.assertEqual(result["target"], "monitor")

    def test_extract_code_delegation(self):
        text = 'DELEGATE_CODE: {"request": "fibonacci", "language": "python"}'
        result = _extract_json_block(text, "DELEGATE_CODE:")
        self.assertIsNotNone(result)
        self.assertEqual(result["request"], "fibonacci")
        self.assertEqual(result["language"], "python")

    def test_no_marker(self):
        result = _extract_json_block("just a normal response", "SCHEDULE:")
        self.assertIsNone(result)

    def test_invalid_json(self):
        text = 'SCHEDULE: {invalid json}'
        result = _extract_json_block(text, "SCHEDULE:")
        self.assertIsNone(result)

    def test_nested_braces(self):
        text = 'SCHEDULE: {"name": "test", "params": {"key": "value"}}'
        result = _extract_json_block(text, "SCHEDULE:")
        self.assertIsNotNone(result)
        self.assertEqual(result["params"]["key"], "value")

    def test_text_after_json(self):
        text = 'SCHEDULE: {"name": "x", "cron": "0 * * * *"} and more text'
        result = _extract_json_block(text, "SCHEDULE:")
        self.assertIsNotNone(result)
        self.assertEqual(result["name"], "x")


class TestAssistantAgent(unittest.TestCase):
    """Tests for AssistantAgent.handle_task()."""

    def setUp(self):
        self.agent = AssistantAgent()
        self.agent.llm = MagicMock()
        self.agent._core = MagicMock()
        self.agent._system_prompt = "test prompt"

    def _run(self, coro):
        return asyncio.get_event_loop().run_until_complete(coro)

    def test_empty_message_greeting(self):
        """Empty message returns greeting."""
        task = _mock_task(message="")
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))
        self.assertEqual(result.exit_code, 0)
        self.assertIn("Hello", result.output)

    def test_basic_chat(self):
        """Basic chat returns LLM response."""
        self.agent.llm.chat = AsyncMock(return_value="The answer is 42.")

        task = _mock_task(message="What is the meaning of life?")
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.exit_code, 0)
        self.assertIn("42", result.output)
        self.agent.llm.chat.assert_called_once()

    def test_history_passed_to_llm(self):
        """Chat history is included in LLM messages."""
        self.agent.llm.chat = AsyncMock(return_value="response")

        history = [
            {"role": "user", "content": "Hi"},
            {"role": "assistant", "content": "Hello!"},
        ]
        task = _mock_task(message="Follow up", history=history)
        ctx = _mock_ctx()
        self._run(self.agent.handle_task(task, ctx))

        call_args = self.agent.llm.chat.call_args
        messages = call_args[0][0]
        # system + 2 history + 1 user = 4 messages
        self.assertEqual(len(messages), 4)
        self.assertEqual(messages[0]["role"], "system")
        self.assertEqual(messages[1]["content"], "Hi")
        self.assertEqual(messages[2]["content"], "Hello!")
        self.assertEqual(messages[3]["content"], "Follow up")

    def test_llm_error_returns_error(self):
        """LLM failure returns error TaskResult."""
        self.agent.llm.chat = AsyncMock(side_effect=RuntimeError("API down"))

        task = _mock_task(message="test")
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.exit_code, 1)
        self.assertIn("LLM error", result.output)

    def test_schedule_detection(self):
        """Response with SCHEDULE: JSON creates cron entry."""
        llm_response = (
            'I will schedule that for you.\n'
            'SCHEDULE: {"name": "github-check", "cron": "*/30 * * * *", '
            '"target": "github-monitor", "description": "Check repos"}'
        )
        self.agent.llm.chat = AsyncMock(return_value=llm_response)
        self.agent.core.add_cron = AsyncMock(return_value="cron-1")

        sibling_pids = {"github-monitor": 42}
        task = _mock_task(message="Schedule GitHub checks", sibling_pids=sibling_pids)
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.exit_code, 0)
        self.assertIn("Scheduled", result.output)
        self.agent.core.add_cron.assert_called_once()
        call_kwargs = self.agent.core.add_cron.call_args.kwargs
        self.assertEqual(call_kwargs["name"], "github-check")
        self.assertEqual(call_kwargs["cron_expression"], "*/30 * * * *")
        self.assertEqual(call_kwargs["target_pid"], 42)

    def test_schedule_target_not_found(self):
        """Schedule with unknown target agent shows error."""
        llm_response = 'SCHEDULE: {"name": "x", "cron": "0 * * * *", "target": "unknown-agent"}'
        self.agent.llm.chat = AsyncMock(return_value=llm_response)

        task = _mock_task(message="Schedule something", sibling_pids={})
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertIn("Could not find agent", result.output)

    def test_code_delegation(self):
        """Response with DELEGATE_CODE: JSON delegates to coder."""
        llm_response = 'DELEGATE_CODE: {"request": "fibonacci function", "language": "python"}'
        self.agent.llm.chat = AsyncMock(return_value=llm_response)

        sibling_pids = {"coder": 99}
        task = _mock_task(message="Write fibonacci", sibling_pids=sibling_pids)
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.exit_code, 0)
        self.assertIn("Code from Coder", result.output)
        ctx.execute_on.assert_called_once()

    def test_code_delegation_no_coder(self):
        """Code delegation without coder shows error."""
        llm_response = 'DELEGATE_CODE: {"request": "hello world", "language": "python"}'
        self.agent.llm.chat = AsyncMock(return_value=llm_response)

        task = _mock_task(message="Write code", sibling_pids={})
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertIn("Coder agent not available", result.output)

    def test_no_markers_clean_response(self):
        """Response without markers is returned as-is."""
        self.agent.llm.chat = AsyncMock(return_value="Just a plain answer.")

        task = _mock_task(message="What is 2+2?")
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.output, "Just a plain answer.")

    def test_invalid_history_json(self):
        """Invalid history JSON is handled gracefully."""
        self.agent.llm.chat = AsyncMock(return_value="ok")

        task = MagicMock()
        task.description = "test"
        task.params = {
            "message": "test",
            "history": "not-json",
            "sibling_pids": "{}",
        }
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))
        self.assertEqual(result.exit_code, 0)

    def test_partial_match_sibling(self):
        """Schedule target is found by partial name match."""
        llm_response = 'SCHEDULE: {"name": "check", "cron": "0 * * * *", "target": "github"}'
        self.agent.llm.chat = AsyncMock(return_value=llm_response)
        self.agent.core.add_cron = AsyncMock(return_value="cron-2")

        sibling_pids = {"github-monitor": 55}
        task = _mock_task(message="Schedule check", sibling_pids=sibling_pids)
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertIn("Scheduled", result.output)
        call_kwargs = self.agent.core.add_cron.call_args.kwargs
        self.assertEqual(call_kwargs["target_pid"], 55)


if __name__ == "__main__":
    unittest.main()
