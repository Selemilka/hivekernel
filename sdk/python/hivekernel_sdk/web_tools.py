"""Web tools: web_fetch and web_search for agents that need internet access."""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
import urllib.request
import urllib.error
from html.parser import HTMLParser
from typing import Any

from .tools import Tool, ToolContext, ToolResult

logger = logging.getLogger("hivekernel.web_tools")


class _HTMLTextExtractor(HTMLParser):
    """Simple HTML-to-text converter."""

    def __init__(self):
        super().__init__()
        self._parts: list[str] = []
        self._skip = False

    def handle_starttag(self, tag, attrs):
        if tag in ("script", "style", "noscript"):
            self._skip = True

    def handle_endtag(self, tag):
        if tag in ("script", "style", "noscript"):
            self._skip = False
        if tag in ("p", "br", "div", "h1", "h2", "h3", "h4", "h5", "h6", "li", "tr"):
            self._parts.append("\n")

    def handle_data(self, data):
        if not self._skip:
            self._parts.append(data)

    def get_text(self) -> str:
        text = "".join(self._parts)
        # Collapse multiple whitespace/newlines.
        text = re.sub(r"\n{3,}", "\n\n", text)
        text = re.sub(r"[ \t]+", " ", text)
        return text.strip()


def _html_to_text(html: str) -> str:
    """Convert HTML to plain text."""
    parser = _HTMLTextExtractor()
    try:
        parser.feed(html)
        return parser.get_text()
    except Exception:
        # Fallback: strip tags with regex.
        return re.sub(r"<[^>]+>", " ", html).strip()


class WebFetchTool:
    """Fetch a URL and return its text content."""

    @property
    def name(self) -> str:
        return "web_fetch"

    @property
    def description(self) -> str:
        return "Fetch a URL and return its text content (HTML stripped)"

    @property
    def parameters(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "url": {"type": "string", "description": "URL to fetch"},
                "max_length": {
                    "type": "integer",
                    "description": "Max chars to return (default 50000)",
                },
            },
            "required": ["url"],
        }

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        url = args["url"]
        max_length = args.get("max_length", 50000)

        def _fetch():
            req = urllib.request.Request(
                url,
                headers={"User-Agent": "HiveKernel-Agent/1.0"},
            )
            with urllib.request.urlopen(req, timeout=30) as resp:
                content_type = resp.headers.get("Content-Type", "")
                raw = resp.read(max_length + 1024)
                return raw, content_type

        try:
            raw, content_type = await asyncio.to_thread(_fetch)
        except urllib.error.HTTPError as e:
            return ToolResult(content=f"HTTP {e.code}: {e.reason}", is_error=True)
        except Exception as e:
            return ToolResult(content=f"Fetch error: {e}", is_error=True)

        text = raw.decode("utf-8", errors="replace")

        if "html" in content_type.lower():
            text = _html_to_text(text)

        if len(text) > max_length:
            text = text[:max_length] + "\n... (truncated)"

        return ToolResult(content=text)


class WebSearchTool:
    """Search the web using Brave Search API."""

    @property
    def name(self) -> str:
        return "web_search"

    @property
    def description(self) -> str:
        return "Search the web using Brave Search API (requires BRAVE_API_KEY env)"

    @property
    def parameters(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "query": {"type": "string", "description": "Search query"},
                "count": {
                    "type": "integer",
                    "description": "Number of results (default 5, max 20)",
                },
            },
            "required": ["query"],
        }

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        api_key = os.environ.get("BRAVE_API_KEY", "")
        if not api_key:
            return ToolResult(
                content="BRAVE_API_KEY not set. Web search unavailable.",
                is_error=True,
            )

        query = args["query"]
        count = min(args.get("count", 5), 20)

        def _search():
            url = f"https://api.search.brave.com/res/v1/web/search?q={urllib.request.quote(query)}&count={count}"
            req = urllib.request.Request(
                url,
                headers={
                    "Accept": "application/json",
                    "Accept-Encoding": "gzip",
                    "X-Subscription-Token": api_key,
                },
            )
            with urllib.request.urlopen(req, timeout=15) as resp:
                return resp.read().decode("utf-8")

        try:
            raw = await asyncio.to_thread(_search)
            data = json.loads(raw)
        except Exception as e:
            return ToolResult(content=f"Search error: {e}", is_error=True)

        results = []
        for item in data.get("web", {}).get("results", [])[:count]:
            results.append({
                "title": item.get("title", ""),
                "url": item.get("url", ""),
                "snippet": item.get("description", ""),
            })

        return ToolResult(content=json.dumps(results, indent=2))


# Only include WebSearchTool if BRAVE_API_KEY is available at import time.
# Both are always in WEB_TOOLS so they can be selected by name in config.
WEB_TOOLS = [WebFetchTool, WebSearchTool]
