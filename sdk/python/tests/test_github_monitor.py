"""Unit tests for GitHubMonitorAgent.

Run: python sdk/python/tests/test_github_monitor.py -v
"""

import asyncio
import json
import os
import sys
import unittest
from unittest.mock import AsyncMock, MagicMock, patch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from hivekernel_sdk.github_monitor import GitHubMonitorAgent, _http_get_json


# --- Helper factories ---

def _mock_ctx():
    """Create a mock SyscallContext."""
    ctx = AsyncMock()
    ctx.log = AsyncMock()
    ctx.escalate = AsyncMock()
    ctx.store_artifact = AsyncMock()
    return ctx


def _mock_task(repos=None, repo=None):
    """Create a mock task for GitMonitor."""
    task = MagicMock()
    params = {}
    if repos is not None:
        params["repos"] = json.dumps(repos)
    if repo is not None:
        params["repo"] = repo
    task.description = "Check repos"
    task.params = params
    return task


def _fake_commits(shas):
    """Create fake GitHub API commit responses."""
    return [
        {
            "sha": sha,
            "commit": {
                "message": f"Commit {sha[:7]}",
                "author": {"name": "TestUser", "date": "2025-01-15T10:00:00Z"},
            },
        }
        for sha in shas
    ]


class TestGitHubMonitorAgent(unittest.TestCase):
    """Tests for GitHubMonitorAgent.handle_task()."""

    def setUp(self):
        self.agent = GitHubMonitorAgent()
        self.agent.llm = MagicMock()
        self.agent._core = MagicMock()
        self.agent._system_prompt = ""

    def _run(self, coro):
        return asyncio.get_event_loop().run_until_complete(coro)

    @patch("hivekernel_sdk.github_monitor._http_get_json")
    def test_first_run_all_commits_new(self, mock_http):
        """First run (no stored state) — all commits are new."""
        commits = _fake_commits(["aaa1111", "bbb2222", "ccc3333"])

        async def fake_http(url):
            return commits
        mock_http.side_effect = fake_http

        # get_artifact raises (no prior state).
        self.agent._core.get_artifact = AsyncMock(side_effect=RuntimeError("not found"))

        task = _mock_task(repos=["Selemilka/hivekernel"])
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.exit_code, 0)
        self.assertIn("3 new commit", result.output)
        ctx.escalate.assert_called_once()
        ctx.store_artifact.assert_called()  # state + updates stored

    @patch("hivekernel_sdk.github_monitor._http_get_json")
    def test_no_new_commits(self, mock_http):
        """When all commits are already known, report no updates."""
        shas = ["aaa1111", "bbb2222"]
        commits = _fake_commits(shas)

        async def fake_http(url):
            return commits
        mock_http.side_effect = fake_http

        # Return known SHAs as stored state.
        stored = MagicMock()
        stored.content = json.dumps(shas).encode()
        self.agent._core.get_artifact = AsyncMock(return_value=stored)

        task = _mock_task(repos=["Selemilka/hivekernel"])
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.exit_code, 0)
        self.assertIn("No new updates", result.output)
        ctx.escalate.assert_not_called()

    @patch("hivekernel_sdk.github_monitor._http_get_json")
    def test_partial_new_commits(self, mock_http):
        """Some commits are new, some are known."""
        old_shas = ["aaa1111", "bbb2222"]
        new_shas = ["ccc3333", "aaa1111", "bbb2222"]
        commits = _fake_commits(new_shas)

        async def fake_http(url):
            return commits
        mock_http.side_effect = fake_http

        stored = MagicMock()
        stored.content = json.dumps(old_shas).encode()
        self.agent._core.get_artifact = AsyncMock(return_value=stored)

        task = _mock_task(repos=["Selemilka/hivekernel"])
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.exit_code, 0)
        self.assertIn("1 new commit", result.output)
        self.assertIn("ccc3333"[:7], result.output)

    @patch("hivekernel_sdk.github_monitor._http_get_json")
    def test_multiple_repos(self, mock_http):
        """Multiple repos are checked."""
        async def fake_http(url):
            if "repo1" in url:
                return _fake_commits(["aaa1111"])
            elif "repo2" in url:
                return _fake_commits(["bbb2222"])
            return []
        mock_http.side_effect = fake_http

        self.agent._core.get_artifact = AsyncMock(side_effect=RuntimeError("not found"))

        task = _mock_task(repos=["user/repo1", "user/repo2"])
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.exit_code, 0)
        self.assertIn("2 new commit", result.output)

    @patch("hivekernel_sdk.github_monitor._http_get_json")
    def test_default_repos(self, mock_http):
        """No repos specified uses DEFAULT_REPOS."""
        async def fake_http(url):
            self.assertIn("Selemilka/hivekernel", url)
            return _fake_commits(["aaa1111"])
        mock_http.side_effect = fake_http

        self.agent._core.get_artifact = AsyncMock(side_effect=RuntimeError("not found"))

        task = _mock_task()  # no repos param
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))
        self.assertEqual(result.exit_code, 0)

    @patch("hivekernel_sdk.github_monitor._http_get_json")
    def test_http_error_handled(self, mock_http):
        """HTTP errors are caught and reported."""
        async def fake_http(url):
            raise RuntimeError("GitHub API error 403: rate limit")
        mock_http.side_effect = fake_http

        task = _mock_task(repos=["Selemilka/hivekernel"])
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.exit_code, 0)
        self.assertIn("No new updates", result.output)
        self.assertIn("rate limit", result.output)

    @patch("hivekernel_sdk.github_monitor._http_get_json")
    def test_single_repo_param(self, mock_http):
        """Single 'repo' param (not 'repos') works."""
        async def fake_http(url):
            self.assertIn("my-org/my-repo", url)
            return _fake_commits(["xyz9999"])
        mock_http.side_effect = fake_http

        self.agent._core.get_artifact = AsyncMock(side_effect=RuntimeError("not found"))

        task = _mock_task(repo="my-org/my-repo")
        ctx = _mock_ctx()
        result = self._run(self.agent.handle_task(task, ctx))

        self.assertEqual(result.exit_code, 0)
        self.assertIn("1 new commit", result.output)

    @patch("hivekernel_sdk.github_monitor._http_get_json")
    def test_stores_state_artifact(self, mock_http):
        """After check, current SHAs are stored as artifact."""
        shas = ["aaa1111", "bbb2222"]
        commits = _fake_commits(shas)

        async def fake_http(url):
            return commits
        mock_http.side_effect = fake_http

        self.agent._core.get_artifact = AsyncMock(side_effect=RuntimeError("not found"))

        task = _mock_task(repos=["org/repo"])
        ctx = _mock_ctx()
        self._run(self.agent.handle_task(task, ctx))

        # Check that state was stored with correct key.
        store_calls = ctx.store_artifact.call_args_list
        state_call = [c for c in store_calls if "github/state/" in str(c)]
        self.assertTrue(len(state_call) > 0, "Should store state artifact")


class TestFormatUpdates(unittest.TestCase):
    """Test the update formatting."""

    def test_format(self):
        updates = [
            {"repo": "org/repo", "sha": "abc1234", "message": "Fix bug", "author": "Bob", "date": ""},
            {"repo": "org/repo", "sha": "def5678", "message": "Add feature", "author": "Alice", "date": ""},
        ]
        result = GitHubMonitorAgent._format_updates(updates)
        self.assertIn("2 new commit", result)
        self.assertIn("abc1234", result)
        self.assertIn("Fix bug", result)
        self.assertIn("Bob", result)


if __name__ == "__main__":
    unittest.main()
