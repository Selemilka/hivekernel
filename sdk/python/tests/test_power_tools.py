"""Unit tests for power_tools.py -- orchestrate and delegate_async.

Run: python -m pytest sdk/python/tests/test_power_tools.py -v
"""

import json
import os
import sys
import unittest
from unittest.mock import AsyncMock, MagicMock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from hivekernel_sdk.power_tools import (
    POWER_TOOLS,
    DelegateAsyncTool,
    OrchestrateTool,
)
from hivekernel_sdk.tools import Tool, ToolContext


class TestDelegateAsyncTool(unittest.IsolatedAsyncioTestCase):
    async def test_delegate(self):
        core = AsyncMock()
        core.send_message = AsyncMock(return_value="msg-42")
        ctx = ToolContext(pid=1, core=core)

        tool = DelegateAsyncTool()
        result = await tool.execute(ctx, {
            "target_pid": 5,
            "task_description": "do this thing",
        })
        self.assertFalse(result.is_error)
        self.assertIn("msg-42", result.content)
        self.assertIn("PID 5", result.content)

        # Verify IPC message was sent correctly.
        core.send_message.assert_called_once()
        call = core.send_message.call_args
        self.assertEqual(call.kwargs["to_pid"], 5)
        self.assertEqual(call.kwargs["type"], "task_request")
        payload = json.loads(call.kwargs["payload"].decode("utf-8"))
        self.assertEqual(payload["task"], "do this thing")


class TestOrchestrateTool(unittest.IsolatedAsyncioTestCase):
    async def test_no_llm_returns_error(self):
        ctx = ToolContext(pid=1, core=AsyncMock(), llm=None)
        tool = OrchestrateTool()
        result = await tool.execute(ctx, {"task": "do stuff"})
        self.assertTrue(result.is_error)
        self.assertIn("LLM", result.content)

    async def test_orchestrate_flow(self):
        """Test the full orchestrate flow with mocked LLM and core."""
        llm = AsyncMock()
        # First call: decompose -> returns JSON array of subtasks.
        # Second call: synthesize -> returns final answer.
        llm.complete = AsyncMock(side_effect=[
            '["subtask A", "subtask B"]',
            "Synthesized final answer combining A and B",
        ])

        core = AsyncMock()
        # spawn_child returns incrementing PIDs.
        core.spawn_child = AsyncMock(side_effect=[100, 101])
        # execute_task returns results.
        core.execute_task = AsyncMock(side_effect=[
            {"exit_code": 0, "output": "Result A"},
            {"exit_code": 0, "output": "Result B"},
        ])
        core.kill_child = AsyncMock()

        ctx = ToolContext(pid=1, core=core, llm=llm)
        tool = OrchestrateTool()
        result = await tool.execute(ctx, {"task": "complex task", "max_workers": 3})

        self.assertFalse(result.is_error)
        self.assertIn("Synthesized", result.content)

        # Verify workers were spawned and killed.
        self.assertEqual(core.spawn_child.call_count, 2)
        self.assertEqual(core.execute_task.call_count, 2)
        self.assertEqual(core.kill_child.call_count, 2)

    async def test_orchestrate_bad_decomposition(self):
        """If LLM returns non-JSON, should error gracefully."""
        llm = AsyncMock()
        llm.complete = AsyncMock(return_value="I cannot decompose this task")

        ctx = ToolContext(pid=1, core=AsyncMock(), llm=llm)
        tool = OrchestrateTool()
        result = await tool.execute(ctx, {"task": "anything"})
        self.assertTrue(result.is_error)
        self.assertIn("JSON", result.content)

    async def test_orchestrate_workers_killed_on_failure(self):
        """Workers should be killed even if execution fails."""
        llm = AsyncMock()
        llm.complete = AsyncMock(side_effect=[
            '["subtask A"]',
            "synthesis",
        ])

        core = AsyncMock()
        core.spawn_child = AsyncMock(return_value=100)
        core.execute_task = AsyncMock(side_effect=Exception("exec failed"))
        core.kill_child = AsyncMock()

        ctx = ToolContext(pid=1, core=core, llm=llm)
        tool = OrchestrateTool()
        # The gather will catch the exception in _run_subtask.
        result = await tool.execute(ctx, {"task": "failing task"})

        # Workers should still be killed.
        core.kill_child.assert_called_once_with(100)


class TestPowerToolsRegistry(unittest.TestCase):
    def test_all_implement_tool_protocol(self):
        for cls in POWER_TOOLS:
            self.assertIsInstance(cls(), Tool)

    def test_count(self):
        self.assertEqual(len(POWER_TOOLS), 2)


if __name__ == "__main__":
    unittest.main()
