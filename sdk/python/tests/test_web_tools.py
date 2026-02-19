"""Unit tests for web_tools.py -- web_fetch and web_search.

Run: python -m pytest sdk/python/tests/test_web_tools.py -v
"""

import json
import os
import sys
import unittest
from unittest.mock import AsyncMock, MagicMock, patch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from hivekernel_sdk.web_tools import (
    WEB_TOOLS,
    WebFetchTool,
    WebSearchTool,
    _html_to_text,
)
from hivekernel_sdk.tools import Tool, ToolContext


class TestHtmlToText(unittest.TestCase):
    def test_simple_html(self):
        html = "<html><body><p>Hello <b>world</b></p></body></html>"
        text = _html_to_text(html)
        self.assertIn("Hello", text)
        self.assertIn("world", text)
        self.assertNotIn("<", text)

    def test_strips_script(self):
        html = "<p>text</p><script>alert(1)</script><p>more</p>"
        text = _html_to_text(html)
        self.assertIn("text", text)
        self.assertIn("more", text)
        self.assertNotIn("alert", text)

    def test_strips_style(self):
        html = "<style>body{color:red}</style><p>visible</p>"
        text = _html_to_text(html)
        self.assertIn("visible", text)
        self.assertNotIn("color", text)

    def test_plain_text_passthrough(self):
        text = _html_to_text("just plain text")
        self.assertEqual(text, "just plain text")


class TestWebFetchTool(unittest.IsolatedAsyncioTestCase):
    @patch("hivekernel_sdk.web_tools.asyncio.to_thread")
    async def test_fetch_html(self, mock_thread):
        mock_thread.return_value = (
            b"<html><body><p>Hello World</p></body></html>",
            "text/html; charset=utf-8",
        )

        ctx = ToolContext(pid=1, core=AsyncMock())
        tool = WebFetchTool()
        result = await tool.execute(ctx, {"url": "https://example.com"})

        self.assertFalse(result.is_error)
        self.assertIn("Hello World", result.content)
        self.assertNotIn("<html>", result.content)

    @patch("hivekernel_sdk.web_tools.asyncio.to_thread")
    async def test_fetch_plain_text(self, mock_thread):
        mock_thread.return_value = (
            b"Just plain text content",
            "text/plain",
        )

        ctx = ToolContext(pid=1, core=AsyncMock())
        tool = WebFetchTool()
        result = await tool.execute(ctx, {"url": "https://example.com/text"})

        self.assertFalse(result.is_error)
        self.assertEqual(result.content, "Just plain text content")

    @patch("hivekernel_sdk.web_tools.asyncio.to_thread")
    async def test_fetch_truncation(self, mock_thread):
        mock_thread.return_value = (b"x" * 60000, "text/plain")

        ctx = ToolContext(pid=1, core=AsyncMock())
        tool = WebFetchTool()
        result = await tool.execute(ctx, {"url": "https://example.com", "max_length": 100})

        self.assertIn("truncated", result.content)
        self.assertLessEqual(len(result.content), 200)  # 100 + truncation message

    @patch("hivekernel_sdk.web_tools.asyncio.to_thread")
    async def test_fetch_error(self, mock_thread):
        mock_thread.side_effect = Exception("Connection refused")

        ctx = ToolContext(pid=1, core=AsyncMock())
        tool = WebFetchTool()
        result = await tool.execute(ctx, {"url": "https://bad.example.com"})

        self.assertTrue(result.is_error)
        self.assertIn("Fetch error", result.content)


class TestWebSearchTool(unittest.IsolatedAsyncioTestCase):
    async def test_no_api_key(self):
        with patch.dict(os.environ, {}, clear=True):
            os.environ.pop("BRAVE_API_KEY", None)
            ctx = ToolContext(pid=1, core=AsyncMock())
            tool = WebSearchTool()
            result = await tool.execute(ctx, {"query": "test"})
            self.assertTrue(result.is_error)
            self.assertIn("BRAVE_API_KEY", result.content)

    @patch("hivekernel_sdk.web_tools.asyncio.to_thread")
    @patch.dict(os.environ, {"BRAVE_API_KEY": "test-key"})
    async def test_search_success(self, mock_thread):
        mock_thread.return_value = json.dumps({
            "web": {
                "results": [
                    {
                        "title": "Test Result",
                        "url": "https://example.com",
                        "description": "A test snippet",
                    }
                ]
            }
        })

        ctx = ToolContext(pid=1, core=AsyncMock())
        tool = WebSearchTool()
        result = await tool.execute(ctx, {"query": "test query"})

        self.assertFalse(result.is_error)
        parsed = json.loads(result.content)
        self.assertEqual(len(parsed), 1)
        self.assertEqual(parsed[0]["title"], "Test Result")

    @patch("hivekernel_sdk.web_tools.asyncio.to_thread")
    @patch.dict(os.environ, {"BRAVE_API_KEY": "test-key"})
    async def test_search_error(self, mock_thread):
        mock_thread.side_effect = Exception("API error")

        ctx = ToolContext(pid=1, core=AsyncMock())
        tool = WebSearchTool()
        result = await tool.execute(ctx, {"query": "test"})
        self.assertTrue(result.is_error)
        self.assertIn("Search error", result.content)


class TestWebToolsRegistry(unittest.TestCase):
    def test_all_implement_tool_protocol(self):
        for cls in WEB_TOOLS:
            self.assertIsInstance(cls(), Tool)

    def test_count(self):
        self.assertEqual(len(WEB_TOOLS), 2)


if __name__ == "__main__":
    unittest.main()
