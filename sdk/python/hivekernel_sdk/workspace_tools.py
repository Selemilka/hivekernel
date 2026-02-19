"""Workspace tools: file operations and shell execution within a sandboxed workspace."""

from __future__ import annotations

import asyncio
import logging
import os
from typing import Any

from .tools import Tool, ToolContext, ToolResult

logger = logging.getLogger("hivekernel.workspace_tools")

MAX_FILE_SIZE = 50 * 1024  # 50KB


def _safe_path(workspace: str, relative: str) -> str:
    """Resolve path and verify it's within workspace. Raises ValueError on escape."""
    if not workspace:
        raise ValueError("No workspace configured for this agent")
    resolved = os.path.realpath(os.path.join(workspace, relative))
    ws_real = os.path.realpath(workspace)
    if not resolved.startswith(ws_real + os.sep) and resolved != ws_real:
        raise ValueError(f"Path escapes workspace: {relative}")
    return resolved


class ReadFileTool:
    """Read a file from the workspace."""

    @property
    def name(self) -> str:
        return "read_file"

    @property
    def description(self) -> str:
        return "Read a file from the workspace directory (max 50KB)"

    @property
    def parameters(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "path": {"type": "string", "description": "Relative path within workspace"},
            },
            "required": ["path"],
        }

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        try:
            fpath = _safe_path(ctx.workspace, args["path"])
        except ValueError as e:
            return ToolResult(content=str(e), is_error=True)

        try:
            with open(fpath, "r", encoding="utf-8", errors="replace") as f:
                content = f.read(MAX_FILE_SIZE)
            if os.path.getsize(fpath) > MAX_FILE_SIZE:
                content += "\n... (truncated at 50KB)"
            return ToolResult(content=content)
        except FileNotFoundError:
            return ToolResult(content=f"File not found: {args['path']}", is_error=True)
        except Exception as e:
            return ToolResult(content=f"Read error: {e}", is_error=True)


class WriteFileTool:
    """Write/create a file in the workspace."""

    @property
    def name(self) -> str:
        return "write_file"

    @property
    def description(self) -> str:
        return "Write or create a file in the workspace directory (creates parent dirs)"

    @property
    def parameters(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "path": {"type": "string", "description": "Relative path within workspace"},
                "content": {"type": "string", "description": "File content to write"},
            },
            "required": ["path", "content"],
        }

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        try:
            fpath = _safe_path(ctx.workspace, args["path"])
        except ValueError as e:
            return ToolResult(content=str(e), is_error=True)

        try:
            os.makedirs(os.path.dirname(fpath), exist_ok=True)
            with open(fpath, "w", encoding="utf-8") as f:
                f.write(args["content"])
            return ToolResult(content=f"Written {len(args['content'])} chars to {args['path']}")
        except Exception as e:
            return ToolResult(content=f"Write error: {e}", is_error=True)


class EditFileTool:
    """Search and replace text in a workspace file."""

    @property
    def name(self) -> str:
        return "edit_file"

    @property
    def description(self) -> str:
        return "Search and replace text in a file within the workspace"

    @property
    def parameters(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "path": {"type": "string", "description": "Relative path within workspace"},
                "old_text": {"type": "string", "description": "Text to find"},
                "new_text": {"type": "string", "description": "Replacement text"},
            },
            "required": ["path", "old_text", "new_text"],
        }

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        try:
            fpath = _safe_path(ctx.workspace, args["path"])
        except ValueError as e:
            return ToolResult(content=str(e), is_error=True)

        try:
            with open(fpath, "r", encoding="utf-8") as f:
                content = f.read()
        except FileNotFoundError:
            return ToolResult(content=f"File not found: {args['path']}", is_error=True)
        except Exception as e:
            return ToolResult(content=f"Read error: {e}", is_error=True)

        old_text = args["old_text"]
        if old_text not in content:
            return ToolResult(content="old_text not found in file", is_error=True)

        count = content.count(old_text)
        new_content = content.replace(old_text, args["new_text"])
        try:
            with open(fpath, "w", encoding="utf-8") as f:
                f.write(new_content)
            return ToolResult(content=f"Replaced {count} occurrence(s) in {args['path']}")
        except Exception as e:
            return ToolResult(content=f"Write error: {e}", is_error=True)


class ListDirTool:
    """List directory contents within the workspace."""

    @property
    def name(self) -> str:
        return "list_dir"

    @property
    def description(self) -> str:
        return "List files and directories in a workspace path"

    @property
    def parameters(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "path": {
                    "type": "string",
                    "description": "Relative path within workspace (default: root)",
                },
            },
        }

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        rel = args.get("path", ".")
        try:
            dirpath = _safe_path(ctx.workspace, rel)
        except ValueError as e:
            return ToolResult(content=str(e), is_error=True)

        if not os.path.isdir(dirpath):
            return ToolResult(content=f"Not a directory: {rel}", is_error=True)

        try:
            entries = []
            for entry in sorted(os.listdir(dirpath)):
                full = os.path.join(dirpath, entry)
                if os.path.isdir(full):
                    entries.append(f"  {entry}/")
                else:
                    size = os.path.getsize(full)
                    entries.append(f"  {entry} ({size} bytes)")
            return ToolResult(content="\n".join(entries) if entries else "(empty)")
        except Exception as e:
            return ToolResult(content=f"List error: {e}", is_error=True)


class ShellExecTool:
    """Run a shell command in the workspace directory."""

    @property
    def name(self) -> str:
        return "shell_exec"

    @property
    def description(self) -> str:
        return "Run a shell command in the workspace directory with timeout"

    @property
    def parameters(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "command": {"type": "string", "description": "Shell command to execute"},
                "timeout_seconds": {
                    "type": "integer",
                    "description": "Timeout in seconds (default 30)",
                },
            },
            "required": ["command"],
        }

    async def execute(self, ctx: ToolContext, args: dict[str, Any]) -> ToolResult:
        if not ctx.workspace:
            return ToolResult(content="No workspace configured", is_error=True)

        command = args["command"]
        timeout = args.get("timeout_seconds", 30)

        try:
            proc = await asyncio.create_subprocess_shell(
                command,
                cwd=ctx.workspace,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
                stdin=asyncio.subprocess.DEVNULL,
            )
            stdout, stderr = await asyncio.wait_for(
                proc.communicate(), timeout=timeout
            )
        except asyncio.TimeoutError:
            try:
                proc.kill()
            except Exception:
                pass
            return ToolResult(
                content=f"Command timed out after {timeout}s", is_error=True
            )
        except Exception as e:
            return ToolResult(content=f"Exec error: {e}", is_error=True)

        out = stdout.decode("utf-8", errors="replace")[:MAX_FILE_SIZE]
        err = stderr.decode("utf-8", errors="replace")[:MAX_FILE_SIZE]

        parts = []
        if out:
            parts.append(out)
        if err:
            parts.append(f"STDERR:\n{err}")
        parts.append(f"(exit code: {proc.returncode})")

        return ToolResult(
            content="\n".join(parts),
            is_error=proc.returncode != 0,
        )


WORKSPACE_TOOLS = [ReadFileTool, WriteFileTool, EditFileTool, ListDirTool, ShellExecTool]
