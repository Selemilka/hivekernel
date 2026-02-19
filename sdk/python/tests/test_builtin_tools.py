"""Unit tests for builtin_tools.py -- syscall tools.

Run: python -m pytest sdk/python/tests/test_builtin_tools.py -v
"""

import json
import os
import sys
import unittest
from unittest.mock import AsyncMock, MagicMock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from hivekernel_sdk.builtin_tools import (
    ALL_BUILTIN_TOOLS,
    ExecuteTaskTool,
    GetArtifactTool,
    GetProcessInfoTool,
    ListChildrenTool,
    ListSiblingsTool,
    MemoryRecallTool,
    MemoryStoreTool,
    ScheduleCronTool,
    SendMessageTool,
    SpawnChildTool,
    StoreArtifactTool,
    register_builtin_tools,
)
from hivekernel_sdk.tools import Tool, ToolContext, ToolRegistry


class TestSpawnChildTool(unittest.IsolatedAsyncioTestCase):
    async def test_spawn(self):
        core = AsyncMock()
        core.spawn_child = AsyncMock(return_value=42)
        ctx = ToolContext(pid=1, core=core)

        tool = SpawnChildTool()
        result = await tool.execute(ctx, {
            "name": "worker-1",
            "role": "worker",
            "cognitive_tier": "operational",
        })
        self.assertIn("42", result.content)
        core.spawn_child.assert_called_once()


class TestExecuteTaskTool(unittest.IsolatedAsyncioTestCase):
    async def test_execute(self):
        core = AsyncMock()
        core.execute_task = AsyncMock(return_value={"exit_code": 0, "output": "done"})
        ctx = ToolContext(pid=1, core=core)

        tool = ExecuteTaskTool()
        result = await tool.execute(ctx, {
            "target_pid": 42,
            "description": "do stuff",
        })
        parsed = json.loads(result.content)
        self.assertEqual(parsed["exit_code"], 0)
        core.execute_task.assert_called_once()


class TestSendMessageTool(unittest.IsolatedAsyncioTestCase):
    async def test_send(self):
        core = AsyncMock()
        core.send_message = AsyncMock(return_value="msg-1")
        ctx = ToolContext(pid=1, core=core)

        tool = SendMessageTool()
        result = await tool.execute(ctx, {
            "to_pid": 2,
            "type": "info",
            "payload": "hello",
        })
        self.assertIn("msg-1", result.content)
        core.send_message.assert_called_once()


class TestStoreArtifactTool(unittest.IsolatedAsyncioTestCase):
    async def test_store(self):
        core = AsyncMock()
        core.store_artifact = AsyncMock(return_value="art-1")
        ctx = ToolContext(pid=1, core=core)

        tool = StoreArtifactTool()
        result = await tool.execute(ctx, {"key": "data", "content": "hello"})
        self.assertIn("art-1", result.content)
        core.store_artifact.assert_called_once_with(
            key="data", content=b"hello"
        )


class TestGetArtifactTool(unittest.IsolatedAsyncioTestCase):
    async def test_get_with_content_attr(self):
        core = AsyncMock()
        art = MagicMock()
        art.content = b"stored data"
        core.get_artifact = AsyncMock(return_value=art)
        ctx = ToolContext(pid=1, core=core)

        tool = GetArtifactTool()
        result = await tool.execute(ctx, {"key": "data"})
        self.assertEqual(result.content, "stored data")
        self.assertFalse(result.is_error)

    async def test_get_bytes(self):
        core = AsyncMock()
        core.get_artifact = AsyncMock(return_value=b"raw bytes")
        ctx = ToolContext(pid=1, core=core)

        tool = GetArtifactTool()
        result = await tool.execute(ctx, {"key": "data"})
        self.assertEqual(result.content, "raw bytes")


class TestListChildrenTool(unittest.IsolatedAsyncioTestCase):
    async def test_list(self):
        core = AsyncMock()
        core.list_children = AsyncMock(return_value=[{"pid": 5, "name": "w1"}])
        ctx = ToolContext(pid=1, core=core)

        tool = ListChildrenTool()
        result = await tool.execute(ctx, {})
        parsed = json.loads(result.content)
        self.assertEqual(len(parsed), 1)


class TestListSiblingsTool(unittest.IsolatedAsyncioTestCase):
    async def test_list(self):
        core = AsyncMock()
        core.list_siblings = AsyncMock(return_value=[{"pid": 3, "name": "sib"}])
        ctx = ToolContext(pid=1, core=core)

        tool = ListSiblingsTool()
        result = await tool.execute(ctx, {})
        parsed = json.loads(result.content)
        self.assertEqual(parsed[0]["pid"], 3)


class TestMemoryStoreTool(unittest.IsolatedAsyncioTestCase):
    async def test_store(self):
        core = AsyncMock()
        core.store_artifact = AsyncMock(return_value="art-1")
        ctx = ToolContext(pid=7, core=core)

        tool = MemoryStoreTool()
        result = await tool.execute(ctx, {"content": "remember this"})
        self.assertIn("saved", result.content)
        core.store_artifact.assert_called_once_with(
            key="agent:7:memory", content=b"remember this"
        )


class TestMemoryRecallTool(unittest.IsolatedAsyncioTestCase):
    async def test_recall_with_content(self):
        core = AsyncMock()
        art = MagicMock()
        art.content = b"remembered stuff"
        core.get_artifact = AsyncMock(return_value=art)
        ctx = ToolContext(pid=7, core=core)

        tool = MemoryRecallTool()
        result = await tool.execute(ctx, {})
        self.assertEqual(result.content, "remembered stuff")

    async def test_recall_empty(self):
        core = AsyncMock()
        core.get_artifact = AsyncMock(return_value=None)
        ctx = ToolContext(pid=7, core=core)

        tool = MemoryRecallTool()
        result = await tool.execute(ctx, {})
        self.assertIn("No memory", result.content)


class TestScheduleCronTool(unittest.IsolatedAsyncioTestCase):
    async def test_schedule(self):
        core = AsyncMock()
        core.add_cron = AsyncMock(return_value="cron-1")
        ctx = ToolContext(pid=1, core=core)

        tool = ScheduleCronTool()
        result = await tool.execute(ctx, {
            "name": "health-check",
            "cron_expression": "*/5 * * * *",
            "target_pid": 3,
            "description": "Check health",
        })
        self.assertIn("cron-1", result.content)
        core.add_cron.assert_called_once()


class TestGetProcessInfoTool(unittest.IsolatedAsyncioTestCase):
    async def test_get_info(self):
        core = AsyncMock()
        info = MagicMock()
        info.pid = 5
        info.ppid = 1
        info.name = "worker-1"
        info.role = "worker"
        info.state = "running"
        info.cognitive_tier = "operational"
        info.model = "mini"
        core.get_process_info = AsyncMock(return_value=info)
        ctx = ToolContext(pid=1, core=core)

        tool = GetProcessInfoTool()
        result = await tool.execute(ctx, {"pid": 5})
        parsed = json.loads(result.content)
        self.assertEqual(parsed["pid"], 5)
        self.assertEqual(parsed["name"], "worker-1")
        core.get_process_info.assert_called_once_with(5)


class TestRegisterBuiltinTools(unittest.TestCase):
    def test_registers_all(self):
        reg = ToolRegistry()
        register_builtin_tools(reg)
        self.assertEqual(len(ALL_BUILTIN_TOOLS), 11)
        for cls in ALL_BUILTIN_TOOLS:
            tool = cls()
            self.assertIsNotNone(reg.get(tool.name))

    def test_all_have_valid_schema(self):
        reg = ToolRegistry()
        register_builtin_tools(reg)
        schema = reg.to_openai_schema()
        self.assertEqual(len(schema), 11)
        for entry in schema:
            self.assertEqual(entry["type"], "function")
            func = entry["function"]
            self.assertIn("name", func)
            self.assertIn("description", func)
            self.assertIn("parameters", func)
            self.assertIn("type", func["parameters"])

    def test_all_implement_tool_protocol(self):
        for cls in ALL_BUILTIN_TOOLS:
            self.assertIsInstance(cls(), Tool)


if __name__ == "__main__":
    unittest.main()
