"""GitHubMonitorAgent -- monitors GitHub repos for new commits.

Triggered by cron via execute_on. Each run:
1. Fetch latest commits from configured repos (unauthenticated GitHub API)
2. Compare with stored state (artifact)
3. If new commits: store update, escalate to parent

Rate limit: 60 req/hour unauthenticated (enough for a few repos).

Runtime image: hivekernel_sdk.github_monitor:GitHubMonitorAgent
"""

import asyncio
import json
import logging
import urllib.request
import urllib.error

from .llm_agent import LLMAgent
from .syscall import SyscallContext
from .types import TaskResult

logger = logging.getLogger("hivekernel.github_monitor")

DEFAULT_REPOS = ["Selemilka/hivekernel"]
COMMITS_PER_PAGE = 5
HTTP_TIMEOUT = 15


class GitHubMonitorAgent(LLMAgent):
    """Monitors GitHub repos for new commits. Daemon, triggered by cron."""

    async def handle_task(self, task, ctx: SyscallContext) -> TaskResult:
        repos_raw = task.params.get("repos", "")
        if repos_raw:
            try:
                repos = json.loads(repos_raw)
            except (json.JSONDecodeError, TypeError):
                repos = [repos_raw]
        else:
            repo_single = task.params.get("repo", "")
            repos = [repo_single] if repo_single else DEFAULT_REPOS

        await ctx.log("info", f"GitMonitor checking {len(repos)} repo(s): {', '.join(repos)}")

        all_updates = []
        errors = []
        for repo in repos:
            try:
                updates = await self._check_repo(repo, ctx)
                all_updates.extend(updates)
            except Exception as e:
                logger.warning("Failed to check %s: %s", repo, e)
                errors.append(f"{repo}: {e}")

        if all_updates:
            summary = self._format_updates(all_updates)
            await ctx.escalate(
                severity="info",
                issue=f"GitHub updates: {len(all_updates)} new commit(s)",
            )
            await ctx.store_artifact(
                key="github/latest-updates",
                content=json.dumps(all_updates, ensure_ascii=False).encode("utf-8"),
                content_type="application/json",
            )
            await ctx.log("info", f"GitMonitor found {len(all_updates)} new commit(s)")
            return TaskResult(exit_code=0, output=summary)

        msg = "No new updates"
        if errors:
            msg += f" (errors: {'; '.join(errors)})"
        await ctx.log("info", msg)
        return TaskResult(exit_code=0, output=msg)

    async def _check_repo(self, repo: str, ctx: SyscallContext) -> list[dict]:
        """Fetch latest commits, compare with stored state, return new ones."""
        url = f"https://api.github.com/repos/{repo}/commits?per_page={COMMITS_PER_PAGE}"
        commits = await _http_get_json(url)

        if not isinstance(commits, list):
            logger.warning("Unexpected response from GitHub for %s: %s", repo, type(commits))
            return []

        # Load previously known SHAs.
        known_shas = set()
        try:
            artifact = await self.core.get_artifact(key=f"github/state/{repo}")
            known_shas = set(json.loads(artifact.content.decode("utf-8")))
        except Exception:
            pass  # First run or artifact not found.

        # Find new commits.
        new_commits = [c for c in commits if c.get("sha", "") not in known_shas]

        # Store current SHAs.
        current_shas = [c["sha"] for c in commits if "sha" in c]
        try:
            await ctx.store_artifact(
                key=f"github/state/{repo}",
                content=json.dumps(current_shas).encode("utf-8"),
                content_type="application/json",
            )
        except Exception as e:
            logger.warning("Failed to store state for %s: %s", repo, e)

        return [
            {
                "repo": repo,
                "sha": c.get("sha", "")[:7],
                "message": c.get("commit", {}).get("message", "").split("\n")[0],
                "author": c.get("commit", {}).get("author", {}).get("name", "unknown"),
                "date": c.get("commit", {}).get("author", {}).get("date", ""),
            }
            for c in new_commits
        ]

    @staticmethod
    def _format_updates(updates: list[dict]) -> str:
        """Format updates into a human-readable summary."""
        lines = [f"Found {len(updates)} new commit(s):", ""]
        for u in updates:
            lines.append(f"  [{u['repo']}] {u['sha']} - {u['message']} ({u['author']})")
        return "\n".join(lines)


async def _http_get_json(url: str) -> list | dict:
    """Async HTTP GET returning parsed JSON. Uses urllib (no deps)."""
    req = urllib.request.Request(url, headers={
        "Accept": "application/vnd.github.v3+json",
        "User-Agent": "HiveKernel-GitMonitor/1.0",
    })

    def _fetch():
        try:
            with urllib.request.urlopen(req, timeout=HTTP_TIMEOUT) as resp:
                return json.loads(resp.read().decode("utf-8"))
        except urllib.error.HTTPError as e:
            raise RuntimeError(f"GitHub API error {e.code}: {e.reason}") from e
        except urllib.error.URLError as e:
            raise RuntimeError(f"Network error: {e.reason}") from e

    return await asyncio.to_thread(_fetch)
