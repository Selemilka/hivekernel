"""Unit tests for tools.py -- Tool infrastructure.

Run: python -m pytest sdk/python/tests/test_tools.py -v
"""

import asyncio
import os
import sys
import unittest
from unittest.mock import AsyncMock, MagicMock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from hivekernel_sdk.tools import Tool, ToolContext, ToolRegistry, ToolResult


class MockTool:
    """A concrete tool for testing."""

    @property
    def name(self) -> str:
        return "mock_tool"

    @property
    def description(self) -> str:
        return "A mock tool for testing"

    @property
    def parameters(self) -> dict:
        return {
            "type": "object",
            "properties": {
                "input": {"type": "string", "description": "Input text"},
            },
            "required": ["input"],
        }

    async def execute(self, ctx, args) -> ToolResult:
        return ToolResult(content=f"echo: {args.get('input', '')}")


class FailingTool:
    """A tool that raises an exception."""

    @property
    def name(self) -> str:
        return "failing_tool"

    @property
    def description(self) -> str:
        return "A tool that always fails"

    @property
    def parameters(self) -> dict:
        return {"type": "object", "properties": {}}

    async def execute(self, ctx, args) -> ToolResult:
        raise ValueError("intentional failure")


class TestToolResult(unittest.TestCase):
    def test_default_not_error(self):
        r = ToolResult(content="ok")
        self.assertFalse(r.is_error)
        self.assertEqual(r.content, "ok")

    def test_error_result(self):
        r = ToolResult(content="bad", is_error=True)
        self.assertTrue(r.is_error)


class TestToolProtocol(unittest.TestCase):
    def test_mock_tool_is_tool(self):
        self.assertIsInstance(MockTool(), Tool)

    def test_failing_tool_is_tool(self):
        self.assertIsInstance(FailingTool(), Tool)


class TestToolContext(unittest.IsolatedAsyncioTestCase):
    async def test_log_via_syscall(self):
        ctx = ToolContext(pid=1, core=AsyncMock(), syscall=AsyncMock())
        await ctx.log("info", "hello")
        ctx.syscall.log.assert_called_once_with("info", "hello")
        ctx.core.log.assert_not_called()

    async def test_log_via_core(self):
        ctx = ToolContext(pid=1, core=AsyncMock(), syscall=None)
        await ctx.log("info", "hello")
        ctx.core.log.assert_called_once_with("info", "hello")

    async def test_store_artifact_via_syscall(self):
        ctx = ToolContext(pid=1, core=AsyncMock(), syscall=AsyncMock())
        ctx.syscall.store_artifact = AsyncMock(return_value="art-1")
        result = await ctx.store_artifact("key", b"data")
        self.assertEqual(result, "art-1")
        ctx.syscall.store_artifact.assert_called_once_with("key", b"data")

    async def test_store_artifact_via_core(self):
        ctx = ToolContext(pid=1, core=AsyncMock(), syscall=None)
        ctx.core.store_artifact = AsyncMock(return_value="art-2")
        result = await ctx.store_artifact("key", b"data")
        self.assertEqual(result, "art-2")

    async def test_get_artifact_via_syscall(self):
        ctx = ToolContext(pid=1, core=AsyncMock(), syscall=AsyncMock())
        ctx.syscall.get_artifact = AsyncMock(return_value=b"content")
        result = await ctx.get_artifact("key")
        self.assertEqual(result, b"content")

    async def test_get_artifact_via_core(self):
        ctx = ToolContext(pid=1, core=AsyncMock(), syscall=None)
        ctx.core.get_artifact = AsyncMock(return_value=b"content")
        result = await ctx.get_artifact("key")
        self.assertEqual(result, b"content")


class TestToolRegistry(unittest.IsolatedAsyncioTestCase):
    def test_register_and_get(self):
        reg = ToolRegistry()
        tool = MockTool()
        reg.register(tool)
        self.assertIs(reg.get("mock_tool"), tool)

    def test_get_unknown(self):
        reg = ToolRegistry()
        self.assertIsNone(reg.get("nonexistent"))

    async def test_execute_registered(self):
        reg = ToolRegistry()
        reg.register(MockTool())
        ctx = ToolContext(pid=1, core=MagicMock())
        result = await reg.execute("mock_tool", ctx, {"input": "hello"})
        self.assertEqual(result.content, "echo: hello")
        self.assertFalse(result.is_error)

    async def test_execute_unknown_returns_error(self):
        reg = ToolRegistry()
        ctx = ToolContext(pid=1, core=MagicMock())
        result = await reg.execute("nonexistent", ctx, {})
        self.assertTrue(result.is_error)
        self.assertIn("Unknown tool", result.content)

    async def test_execute_failing_tool_returns_error(self):
        reg = ToolRegistry()
        reg.register(FailingTool())
        ctx = ToolContext(pid=1, core=MagicMock())
        result = await reg.execute("failing_tool", ctx, {})
        self.assertTrue(result.is_error)
        self.assertIn("intentional failure", result.content)

    def test_to_openai_schema(self):
        reg = ToolRegistry()
        reg.register(MockTool())
        schema = reg.to_openai_schema()
        self.assertEqual(len(schema), 1)
        self.assertEqual(schema[0]["type"], "function")
        self.assertEqual(schema[0]["function"]["name"], "mock_tool")
        self.assertEqual(schema[0]["function"]["description"], "A mock tool for testing")
        self.assertIn("properties", schema[0]["function"]["parameters"])

    def test_to_openai_schema_empty(self):
        reg = ToolRegistry()
        schema = reg.to_openai_schema()
        self.assertEqual(schema, [])

    def test_to_openai_schema_multiple(self):
        reg = ToolRegistry()
        reg.register(MockTool())
        reg.register(FailingTool())
        schema = reg.to_openai_schema()
        self.assertEqual(len(schema), 2)
        names = {s["function"]["name"] for s in schema}
        self.assertEqual(names, {"mock_tool", "failing_tool"})


if __name__ == "__main__":
    unittest.main()
