"""Unit tests for workspace_tools.py -- file and shell tools.

Run: python -m pytest sdk/python/tests/test_workspace_tools.py -v
"""

import json
import os
import sys
import tempfile
import unittest
from unittest.mock import AsyncMock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from hivekernel_sdk.workspace_tools import (
    WORKSPACE_TOOLS,
    EditFileTool,
    ListDirTool,
    ReadFileTool,
    ShellExecTool,
    WriteFileTool,
    _safe_path,
)
from hivekernel_sdk.tools import Tool, ToolContext


class TestSafePath(unittest.TestCase):
    def test_valid_relative(self):
        with tempfile.TemporaryDirectory() as ws:
            result = _safe_path(ws, "subdir/file.txt")
            self.assertTrue(result.startswith(os.path.realpath(ws)))

    def test_rejects_parent_traversal(self):
        with tempfile.TemporaryDirectory() as ws:
            with self.assertRaises(ValueError):
                _safe_path(ws, "../../etc/passwd")

    def test_rejects_absolute_outside(self):
        with tempfile.TemporaryDirectory() as ws:
            with self.assertRaises(ValueError):
                _safe_path(ws, "/tmp/outside")

    def test_no_workspace_raises(self):
        with self.assertRaises(ValueError):
            _safe_path("", "file.txt")

    def test_dot_resolves_to_workspace(self):
        with tempfile.TemporaryDirectory() as ws:
            result = _safe_path(ws, ".")
            self.assertEqual(result, os.path.realpath(ws))


class TestReadFileTool(unittest.IsolatedAsyncioTestCase):
    async def test_read_existing_file(self):
        with tempfile.TemporaryDirectory() as ws:
            fpath = os.path.join(ws, "test.txt")
            with open(fpath, "w") as f:
                f.write("hello world")

            ctx = ToolContext(pid=1, core=AsyncMock(), workspace=ws)
            tool = ReadFileTool()
            result = await tool.execute(ctx, {"path": "test.txt"})
            self.assertEqual(result.content, "hello world")
            self.assertFalse(result.is_error)

    async def test_read_nonexistent_file(self):
        with tempfile.TemporaryDirectory() as ws:
            ctx = ToolContext(pid=1, core=AsyncMock(), workspace=ws)
            tool = ReadFileTool()
            result = await tool.execute(ctx, {"path": "nope.txt"})
            self.assertTrue(result.is_error)
            self.assertIn("not found", result.content)

    async def test_read_rejects_escape(self):
        with tempfile.TemporaryDirectory() as ws:
            ctx = ToolContext(pid=1, core=AsyncMock(), workspace=ws)
            tool = ReadFileTool()
            result = await tool.execute(ctx, {"path": "../../etc/passwd"})
            self.assertTrue(result.is_error)
            self.assertIn("escapes", result.content)


class TestWriteFileTool(unittest.IsolatedAsyncioTestCase):
    async def test_write_creates_file(self):
        with tempfile.TemporaryDirectory() as ws:
            ctx = ToolContext(pid=1, core=AsyncMock(), workspace=ws)
            tool = WriteFileTool()
            result = await tool.execute(ctx, {"path": "new.txt", "content": "data"})
            self.assertFalse(result.is_error)

            with open(os.path.join(ws, "new.txt")) as f:
                self.assertEqual(f.read(), "data")

    async def test_write_creates_parent_dirs(self):
        with tempfile.TemporaryDirectory() as ws:
            ctx = ToolContext(pid=1, core=AsyncMock(), workspace=ws)
            tool = WriteFileTool()
            result = await tool.execute(ctx, {"path": "sub/dir/file.txt", "content": "nested"})
            self.assertFalse(result.is_error)
            self.assertTrue(os.path.exists(os.path.join(ws, "sub", "dir", "file.txt")))


class TestEditFileTool(unittest.IsolatedAsyncioTestCase):
    async def test_edit_replaces_text(self):
        with tempfile.TemporaryDirectory() as ws:
            fpath = os.path.join(ws, "edit.txt")
            with open(fpath, "w") as f:
                f.write("hello world")

            ctx = ToolContext(pid=1, core=AsyncMock(), workspace=ws)
            tool = EditFileTool()
            result = await tool.execute(ctx, {
                "path": "edit.txt",
                "old_text": "hello",
                "new_text": "goodbye",
            })
            self.assertFalse(result.is_error)
            self.assertIn("1", result.content)

            with open(fpath) as f:
                self.assertEqual(f.read(), "goodbye world")

    async def test_edit_not_found(self):
        with tempfile.TemporaryDirectory() as ws:
            fpath = os.path.join(ws, "edit.txt")
            with open(fpath, "w") as f:
                f.write("hello world")

            ctx = ToolContext(pid=1, core=AsyncMock(), workspace=ws)
            tool = EditFileTool()
            result = await tool.execute(ctx, {
                "path": "edit.txt",
                "old_text": "missing",
                "new_text": "new",
            })
            self.assertTrue(result.is_error)
            self.assertIn("not found", result.content)


class TestListDirTool(unittest.IsolatedAsyncioTestCase):
    async def test_list_directory(self):
        with tempfile.TemporaryDirectory() as ws:
            os.makedirs(os.path.join(ws, "subdir"))
            with open(os.path.join(ws, "file.txt"), "w") as f:
                f.write("data")

            ctx = ToolContext(pid=1, core=AsyncMock(), workspace=ws)
            tool = ListDirTool()
            result = await tool.execute(ctx, {})
            self.assertFalse(result.is_error)
            self.assertIn("file.txt", result.content)
            self.assertIn("subdir/", result.content)

    async def test_list_empty_dir(self):
        with tempfile.TemporaryDirectory() as ws:
            ctx = ToolContext(pid=1, core=AsyncMock(), workspace=ws)
            tool = ListDirTool()
            result = await tool.execute(ctx, {})
            self.assertEqual(result.content, "(empty)")


class TestShellExecTool(unittest.IsolatedAsyncioTestCase):
    async def test_simple_command(self):
        with tempfile.TemporaryDirectory() as ws:
            ctx = ToolContext(pid=1, core=AsyncMock(), workspace=ws)
            tool = ShellExecTool()
            result = await tool.execute(ctx, {"command": "echo hello"})
            self.assertIn("hello", result.content)
            self.assertFalse(result.is_error)

    async def test_no_workspace(self):
        ctx = ToolContext(pid=1, core=AsyncMock(), workspace="")
        tool = ShellExecTool()
        result = await tool.execute(ctx, {"command": "echo test"})
        self.assertTrue(result.is_error)
        self.assertIn("No workspace", result.content)

    async def test_timeout(self):
        import asyncio
        ws = tempfile.mkdtemp()
        try:
            ctx = ToolContext(pid=1, core=AsyncMock(), workspace=ws)
            tool = ShellExecTool()
            # Use a command that sleeps longer than the timeout.
            result = await tool.execute(ctx, {
                "command": "python -c \"import time; time.sleep(10)\"",
                "timeout_seconds": 1,
            })
            self.assertTrue(result.is_error)
            self.assertIn("timed out", result.content)
            # Give Windows time to release the process handle.
            await asyncio.sleep(0.5)
        finally:
            import shutil
            try:
                shutil.rmtree(ws, ignore_errors=True)
            except Exception:
                pass


class TestWorkspaceToolsRegistry(unittest.TestCase):
    def test_all_implement_tool_protocol(self):
        for cls in WORKSPACE_TOOLS:
            self.assertIsInstance(cls(), Tool)

    def test_count(self):
        self.assertEqual(len(WORKSPACE_TOOLS), 5)


if __name__ == "__main__":
    unittest.main()
