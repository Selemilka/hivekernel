"""Unit tests for loop.py -- AgentLoop.

Run: python -m pytest sdk/python/tests/test_loop.py -v
"""

import json
import os
import sys
import unittest
from unittest.mock import AsyncMock, MagicMock, patch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from hivekernel_sdk.loop import AgentLoop, LoopResult
from hivekernel_sdk.memory import AgentMemory
from hivekernel_sdk.tools import ToolContext, ToolRegistry, ToolResult


class MockLLM:
    """Mock LLMClient with configurable responses."""

    def __init__(self, responses: list[dict]):
        self._responses = list(responses)
        self._call_count = 0
        self._total_tokens = 0

    async def chat_with_tools(self, messages, tools=None, system="",
                               model="", max_tokens=4096, temperature=0.7):
        resp = self._responses[self._call_count]
        self._call_count += 1
        return resp

    async def chat(self, messages, system="", model="", max_tokens=4096,
                   temperature=0.7):
        return "Summary of conversation."


class EchoTool:
    @property
    def name(self):
        return "echo"

    @property
    def description(self):
        return "Echo input"

    @property
    def parameters(self):
        return {"type": "object", "properties": {"text": {"type": "string"}}}

    async def execute(self, ctx, args):
        return ToolResult(content=f"echoed: {args.get('text', '')}")


class FailTool:
    @property
    def name(self):
        return "fail"

    @property
    def description(self):
        return "Always fails"

    @property
    def parameters(self):
        return {"type": "object", "properties": {}}

    async def execute(self, ctx, args):
        raise RuntimeError("tool broke")


def _make_tool_response(tool_calls: list[dict]) -> dict:
    """Create an LLM response with tool calls."""
    return {
        "message": {
            "role": "assistant",
            "content": "",
            "tool_calls": tool_calls,
        },
        "finish_reason": "tool_calls",
    }


def _make_text_response(text: str) -> dict:
    """Create an LLM response with text."""
    return {
        "message": {
            "role": "assistant",
            "content": text,
        },
        "finish_reason": "stop",
    }


def _make_tool_call(name: str, args: dict, tc_id: str = "tc-1") -> dict:
    return {
        "id": tc_id,
        "type": "function",
        "function": {
            "name": name,
            "arguments": json.dumps(args),
        },
    }


class TestAgentLoopBasic(unittest.IsolatedAsyncioTestCase):
    async def test_immediate_text_response(self):
        """LLM returns text immediately -- 1 iteration, 0 tool calls."""
        llm = MockLLM([_make_text_response("Hello there!")])
        reg = ToolRegistry()
        mem = AgentMemory(pid=1)
        ctx = ToolContext(pid=1, core=AsyncMock())

        loop = AgentLoop(llm, reg, mem, max_iterations=5)
        result = await loop.run("hi", ctx, system_prompt="Be helpful")

        self.assertEqual(result.content, "Hello there!")
        self.assertEqual(result.iterations, 1)
        self.assertEqual(result.tool_calls_total, 0)

    async def test_tool_then_text(self):
        """LLM calls a tool, then returns text -- 2 iterations, 1 tool call."""
        llm = MockLLM([
            _make_tool_response([_make_tool_call("echo", {"text": "world"})]),
            _make_text_response("Done echoing"),
        ])
        reg = ToolRegistry()
        reg.register(EchoTool())
        mem = AgentMemory(pid=1)
        ctx = ToolContext(pid=1, core=AsyncMock())

        loop = AgentLoop(llm, reg, mem, max_iterations=5)
        result = await loop.run("echo world", ctx)

        self.assertEqual(result.content, "Done echoing")
        self.assertEqual(result.iterations, 2)
        self.assertEqual(result.tool_calls_total, 1)

    async def test_multiple_tool_calls_then_text(self):
        """LLM calls tools twice, then returns text -- 3 iterations."""
        llm = MockLLM([
            _make_tool_response([_make_tool_call("echo", {"text": "a"}, "tc-1")]),
            _make_tool_response([_make_tool_call("echo", {"text": "b"}, "tc-2")]),
            _make_text_response("All done"),
        ])
        reg = ToolRegistry()
        reg.register(EchoTool())
        mem = AgentMemory(pid=1)
        ctx = ToolContext(pid=1, core=AsyncMock())

        loop = AgentLoop(llm, reg, mem, max_iterations=5)
        result = await loop.run("do things", ctx)

        self.assertEqual(result.content, "All done")
        self.assertEqual(result.iterations, 3)
        self.assertEqual(result.tool_calls_total, 2)


class TestAgentLoopMaxIterations(unittest.IsolatedAsyncioTestCase):
    async def test_max_iterations_stops_loop(self):
        """Loop stops after max_iterations even if LLM keeps calling tools."""
        responses = [
            _make_tool_response([_make_tool_call("echo", {"text": str(i)}, f"tc-{i}")])
            for i in range(10)
        ]
        llm = MockLLM(responses)
        reg = ToolRegistry()
        reg.register(EchoTool())
        mem = AgentMemory(pid=1)
        ctx = ToolContext(pid=1, core=AsyncMock())

        loop = AgentLoop(llm, reg, mem, max_iterations=3)
        result = await loop.run("loop forever", ctx)

        self.assertEqual(result.iterations, 3)
        self.assertEqual(result.tool_calls_total, 3)


class TestAgentLoopErrors(unittest.IsolatedAsyncioTestCase):
    async def test_tool_error_continues_loop(self):
        """Tool execution error is appended and loop continues."""
        llm = MockLLM([
            _make_tool_response([_make_tool_call("fail", {}, "tc-1")]),
            _make_text_response("Handled the error"),
        ])
        reg = ToolRegistry()
        reg.register(FailTool())
        mem = AgentMemory(pid=1)
        ctx = ToolContext(pid=1, core=AsyncMock())

        loop = AgentLoop(llm, reg, mem, max_iterations=5)
        result = await loop.run("try failing", ctx)

        self.assertEqual(result.content, "Handled the error")
        self.assertEqual(result.iterations, 2)
        self.assertEqual(result.tool_calls_total, 1)

    async def test_unknown_tool_returns_error(self):
        """Calling unknown tool returns error result and continues."""
        llm = MockLLM([
            _make_tool_response([_make_tool_call("nonexistent", {}, "tc-1")]),
            _make_text_response("OK"),
        ])
        reg = ToolRegistry()
        mem = AgentMemory(pid=1)
        ctx = ToolContext(pid=1, core=AsyncMock())

        loop = AgentLoop(llm, reg, mem, max_iterations=5)
        result = await loop.run("try unknown", ctx)

        self.assertEqual(result.content, "OK")
        # Check error was recorded in messages
        tool_msgs = [m for m in mem.messages if m["role"] == "tool"]
        self.assertTrue(any("Unknown tool" in m["content"] for m in tool_msgs))

    async def test_invalid_json_args(self):
        """Invalid JSON in tool_call arguments uses empty dict."""
        llm = MockLLM([
            {
                "message": {
                    "role": "assistant",
                    "content": "",
                    "tool_calls": [{
                        "id": "tc-1",
                        "type": "function",
                        "function": {
                            "name": "echo",
                            "arguments": "not valid json{{{",
                        },
                    }],
                },
                "finish_reason": "tool_calls",
            },
            _make_text_response("recovered"),
        ])
        reg = ToolRegistry()
        reg.register(EchoTool())
        mem = AgentMemory(pid=1)
        ctx = ToolContext(pid=1, core=AsyncMock())

        loop = AgentLoop(llm, reg, mem, max_iterations=5)
        result = await loop.run("bad json", ctx)

        self.assertEqual(result.content, "recovered")


class TestAgentLoopMemory(unittest.IsolatedAsyncioTestCase):
    async def test_memory_saved_after_run(self):
        """Memory is saved to context after loop completes."""
        llm = MockLLM([_make_text_response("hi")])
        reg = ToolRegistry()
        mem = AgentMemory(pid=1)
        ctx = ToolContext(pid=1, core=AsyncMock())

        loop = AgentLoop(llm, reg, mem, max_iterations=5)
        await loop.run("hello", ctx)

        ctx.core.store_artifact.assert_called()

    async def test_messages_accumulated(self):
        """User and assistant messages are in memory after run."""
        llm = MockLLM([_make_text_response("response")])
        reg = ToolRegistry()
        mem = AgentMemory(pid=1)
        ctx = ToolContext(pid=1, core=AsyncMock())

        loop = AgentLoop(llm, reg, mem, max_iterations=5)
        await loop.run("question", ctx)

        roles = [m["role"] for m in mem.messages]
        self.assertIn("user", roles)
        self.assertIn("assistant", roles)


if __name__ == "__main__":
    unittest.main()
