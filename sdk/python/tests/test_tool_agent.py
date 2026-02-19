"""Unit tests for tool_agent.py -- ToolAgent base class.

Run: python -m pytest sdk/python/tests/test_tool_agent.py -v
"""

import json
import os
import sys
import unittest
from unittest.mock import AsyncMock, MagicMock, patch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from hivekernel_sdk.tool_agent import ToolAgent
from hivekernel_sdk.tools import Tool, ToolContext, ToolRegistry, ToolResult
from hivekernel_sdk.types import AgentConfig, Message, MessageAck, Task, TaskResult


class CalculatorTool:
    """A custom tool for testing."""

    @property
    def name(self):
        return "calculator"

    @property
    def description(self):
        return "Perform basic math"

    @property
    def parameters(self):
        return {
            "type": "object",
            "properties": {
                "expression": {"type": "string", "description": "Math expression"},
            },
            "required": ["expression"],
        }

    async def execute(self, ctx, args):
        expr = args.get("expression", "0")
        try:
            # Only allow basic math (no builtins)
            result = eval(expr, {"__builtins__": {}})
            return ToolResult(content=str(result))
        except Exception as e:
            return ToolResult(content=f"Error: {e}", is_error=True)


class TestToolAgent(ToolAgent):
    """Concrete subclass for testing."""

    def get_tools(self):
        return [CalculatorTool()]


class TestOnInit(unittest.IsolatedAsyncioTestCase):
    @patch.dict(os.environ, {"OPENROUTER_API_KEY": "test-key"})
    async def test_on_init_registers_builtin_tools(self):
        agent = TestToolAgent()
        agent._pid = 5
        config = AgentConfig(
            name="test",
            system_prompt="Be helpful",
            model="mini",
        )
        await agent.on_init(config)

        # Built-in tools should be registered
        self.assertIsNotNone(agent.registry.get("spawn_child"))
        self.assertIsNotNone(agent.registry.get("store_artifact"))
        self.assertIsNotNone(agent.registry.get("memory_store"))

    @patch.dict(os.environ, {"OPENROUTER_API_KEY": "test-key"})
    async def test_on_init_registers_custom_tools(self):
        agent = TestToolAgent()
        agent._pid = 5
        config = AgentConfig(name="test", model="mini")
        await agent.on_init(config)

        # Custom tool should be registered
        self.assertIsNotNone(agent.registry.get("calculator"))

    @patch.dict(os.environ, {"OPENROUTER_API_KEY": "test-key"})
    async def test_on_init_creates_memory(self):
        agent = TestToolAgent()
        agent._pid = 5
        config = AgentConfig(name="test", model="mini")
        await agent.on_init(config)

        self.assertIsNotNone(agent.memory)
        self.assertEqual(agent.memory.pid, 5)

    @patch.dict(os.environ, {"OPENROUTER_API_KEY": "test-key"})
    async def test_on_init_creates_loop(self):
        agent = TestToolAgent()
        agent._pid = 5
        config = AgentConfig(name="test", model="mini")
        await agent.on_init(config)

        self.assertIsNotNone(agent.agent_loop)

    @patch.dict(os.environ, {"OPENROUTER_API_KEY": "test-key"})
    async def test_max_iterations_from_metadata(self):
        agent = TestToolAgent()
        agent._pid = 5
        config = AgentConfig(
            name="test",
            model="mini",
            metadata={"max_iterations": "25"},
        )
        await agent.on_init(config)

        self.assertEqual(agent.agent_loop.max_iterations, 25)


class TestHandleTask(unittest.IsolatedAsyncioTestCase):
    @patch.dict(os.environ, {"OPENROUTER_API_KEY": "test-key"})
    async def test_handle_task_runs_loop(self):
        agent = TestToolAgent()
        agent._pid = 5
        agent._core = AsyncMock()
        # Mock get_artifact to return nothing (no prior memory)
        agent._core.get_artifact = AsyncMock(side_effect=Exception("not found"))

        config = AgentConfig(name="test", model="mini")
        await agent.on_init(config)

        # Mock the agent loop run method
        agent.agent_loop.run = AsyncMock(return_value=MagicMock(
            content="Task completed",
            iterations=2,
            tool_calls_total=1,
        ))

        task = Task(task_id="t1", description="do something")
        ctx = AsyncMock()

        result = await agent.handle_task(task, ctx)

        self.assertEqual(result.exit_code, 0)
        self.assertEqual(result.output, "Task completed")
        self.assertEqual(result.metadata["iterations"], "2")
        self.assertEqual(result.metadata["tool_calls"], "1")
        agent.agent_loop.run.assert_called_once()

    @patch.dict(os.environ, {"OPENROUTER_API_KEY": "test-key"})
    async def test_handle_task_uses_params_task(self):
        """Should use task.params['task'] over task.description."""
        agent = TestToolAgent()
        agent._pid = 5
        agent._core = AsyncMock()
        agent._core.get_artifact = AsyncMock(side_effect=Exception("not found"))

        config = AgentConfig(name="test", model="mini")
        await agent.on_init(config)

        agent.agent_loop.run = AsyncMock(return_value=MagicMock(
            content="ok", iterations=1, tool_calls_total=0,
        ))

        task = Task(
            task_id="t1",
            description="generic",
            params={"task": "specific instructions"},
        )
        ctx = AsyncMock()
        await agent.handle_task(task, ctx)

        call_args = agent.agent_loop.run.call_args
        self.assertEqual(call_args.kwargs["prompt"], "specific instructions")


class TestHandleMessage(unittest.IsolatedAsyncioTestCase):
    @patch.dict(os.environ, {"OPENROUTER_API_KEY": "test-key"})
    async def test_handle_message_task_request(self):
        agent = TestToolAgent()
        agent._pid = 5
        agent._core = AsyncMock()
        agent._core.get_artifact = AsyncMock(side_effect=Exception("not found"))
        agent._core.send_message = AsyncMock(return_value="msg-1")

        config = AgentConfig(name="test", model="mini")
        await agent.on_init(config)

        agent.agent_loop.run = AsyncMock(return_value=MagicMock(
            content="message response",
            iterations=1,
            tool_calls_total=0,
        ))

        msg = Message(
            message_id="msg-1",
            from_pid=2,
            type="task_request",
            payload=json.dumps({"task": "do this"}).encode("utf-8"),
        )

        ack = await agent.handle_message(msg)
        self.assertEqual(ack.status, MessageAck.ACK_ACCEPTED)
        agent.agent_loop.run.assert_called_once()

    @patch.dict(os.environ, {"OPENROUTER_API_KEY": "test-key"})
    async def test_handle_message_non_task_request(self):
        """Non task_request messages use default handling."""
        agent = TestToolAgent()
        agent._pid = 5
        agent._core = AsyncMock()
        agent._core.get_artifact = AsyncMock(side_effect=Exception("not found"))

        config = AgentConfig(name="test", model="mini")
        await agent.on_init(config)

        msg = Message(
            message_id="msg-1",
            from_pid=2,
            type="cron_task",
            payload=b"{}",
        )

        ack = await agent.handle_message(msg)
        self.assertEqual(ack.status, MessageAck.ACK_ACCEPTED)


class TestGetTools(unittest.TestCase):
    def test_base_returns_empty(self):
        agent = ToolAgent()
        self.assertEqual(agent.get_tools(), [])

    def test_subclass_returns_custom(self):
        agent = TestToolAgent()
        tools = agent.get_tools()
        self.assertEqual(len(tools), 1)
        self.assertEqual(tools[0].name, "calculator")


if __name__ == "__main__":
    unittest.main()
