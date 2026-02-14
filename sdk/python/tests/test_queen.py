"""Unit tests for QueenAgent Phase 3 features.

Run: python sdk/python/tests/test_queen.py -v
"""

import asyncio
import json
import os
import sys
import time
import unittest
from collections import deque
from unittest.mock import AsyncMock, MagicMock, patch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from hivekernel_sdk.queen import (
    QueenAgent,
    _word_similarity,
    _SIMPLE_KEYWORDS,
    _COMPLEX_KEYWORDS,
    SIMILARITY_THRESHOLD,
)


# --- Helper factories ---

def _mock_ctx():
    """Create a mock SyscallContext."""
    ctx = AsyncMock()
    ctx.log = AsyncMock()
    ctx.report_progress = AsyncMock()
    ctx.spawn = AsyncMock(return_value=10)
    ctx.kill = AsyncMock()
    ctx.execute_on = AsyncMock(return_value=MagicMock(
        exit_code=0,
        output="test result",
        artifacts={},
        metadata={},
    ))
    ctx.store_artifact = AsyncMock()
    return ctx


def _mock_task(description="test task", params=None):
    """Create a mock task."""
    task = MagicMock()
    task.description = description
    task.params = params or {"task": description}
    return task


def _mock_process_info(pid, state=1, name="test"):
    """Create a mock ProcessInfo response."""
    info = MagicMock()
    info.pid = pid
    info.state = state
    info.name = name
    return info


# --- Word Similarity Tests ---

class TestWordSimilarity(unittest.TestCase):
    """Test _word_similarity function."""

    def test_identical_strings(self):
        self.assertAlmostEqual(_word_similarity("hello world", "hello world"), 1.0)

    def test_completely_different(self):
        self.assertAlmostEqual(_word_similarity("hello world", "foo bar"), 0.0)

    def test_partial_overlap(self):
        # "hello world" vs "hello there" -> intersection={"hello"}, union={"hello","world","there"}
        sim = _word_similarity("hello world", "hello there")
        self.assertAlmostEqual(sim, 1.0 / 3.0)

    def test_empty_string(self):
        self.assertAlmostEqual(_word_similarity("", "hello"), 0.0)
        self.assertAlmostEqual(_word_similarity("hello", ""), 0.0)
        self.assertAlmostEqual(_word_similarity("", ""), 0.0)

    def test_case_insensitive(self):
        self.assertAlmostEqual(_word_similarity("Hello World", "hello world"), 1.0)

    def test_high_similarity(self):
        sim = _word_similarity(
            "research the topic of AI safety",
            "research the topic of AI alignment",
        )
        # intersection={"research","the","topic","of","ai"}, union has 7 words
        self.assertGreater(sim, 0.5)


# --- Heuristic Complexity Tests ---

class TestHeuristicComplexity(unittest.IsolatedAsyncioTestCase):
    """Test heuristic-based complexity assessment."""

    async def _make_queen(self):
        """Create a QueenAgent with mocked dependencies."""
        queen = QueenAgent()
        queen._core = AsyncMock()
        queen.llm = MagicMock()
        queen.ask = AsyncMock(return_value='{"complexity": "complex"}')
        return queen

    async def test_simple_short_task(self):
        queen = await self._make_queen()
        result = await queen._assess_complexity("summarize this text")
        self.assertEqual(result, "simple")

    async def test_simple_explain(self):
        queen = await self._make_queen()
        result = await queen._assess_complexity("explain quantum physics")
        self.assertEqual(result, "simple")

    async def test_complex_keywords(self):
        queen = await self._make_queen()
        result = await queen._assess_complexity(
            "research and analyze the impact of AI on healthcare"
        )
        self.assertEqual(result, "complex")

    async def test_complex_long_task(self):
        queen = await self._make_queen()
        long_desc = "Do something interesting. " * 20  # > 200 chars
        result = await queen._assess_complexity(long_desc)
        self.assertEqual(result, "complex")

    async def test_ambiguous_falls_to_llm(self):
        queen = await self._make_queen()
        queen.ask = AsyncMock(return_value='{"complexity": "simple"}')
        # "write a poem" -- single complex keyword but short
        result = await queen._assess_complexity("write a poem about nature")
        # Should fall through to LLM (no simple keywords, only 1 complex keyword)
        self.assertEqual(result, "simple")
        queen.ask.assert_called_once()

    async def test_llm_failure_defaults_complex(self):
        queen = await self._make_queen()
        queen.ask = AsyncMock(side_effect=RuntimeError("LLM unavailable"))
        result = await queen._assess_complexity("write a poem about nature")
        self.assertEqual(result, "complex")


# --- Task History Tests ---

class TestTaskHistory(unittest.IsolatedAsyncioTestCase):
    """Test history-based complexity routing."""

    async def _make_queen(self):
        queen = QueenAgent()
        queen._core = AsyncMock()
        queen.llm = MagicMock()
        queen.ask = AsyncMock(return_value='{"complexity": "complex"}')
        return queen

    async def test_history_match_returns_cached(self):
        queen = await self._make_queen()
        queen._task_history.append({
            "task": "research AI safety topics",
            "complexity": "complex",
            "exit_code": 0,
            "ts": time.time(),
        })
        # Similar task should match from history.
        result = await queen._assess_complexity("research AI safety papers")
        self.assertEqual(result, "complex")
        # LLM should NOT be called.
        queen.ask.assert_not_called()

    async def test_history_no_match_falls_through(self):
        queen = await self._make_queen()
        queen._task_history.append({
            "task": "translate this document",
            "complexity": "simple",
            "exit_code": 0,
            "ts": time.time(),
        })
        # Completely different task should not match.
        result = await queen._assess_complexity("research quantum computing")
        # Should hit heuristic (2 complex keywords).
        self.assertEqual(result, "complex")

    async def test_history_recorded_after_task(self):
        queen = await self._make_queen()
        queen._core.get_artifact = AsyncMock(side_effect=RuntimeError("not found"))
        ctx = _mock_ctx()
        task = _mock_task("summarize this article")

        await queen.handle_task(task, ctx)

        self.assertEqual(len(queen._task_history), 1)
        entry = queen._task_history[0]
        self.assertEqual(entry["task"], "summarize this article")
        self.assertEqual(entry["complexity"], "simple")

    async def test_history_max_size(self):
        queen = await self._make_queen()
        # Fill history to max.
        for i in range(25):
            queen._task_history.append({
                "task": f"task number {i}",
                "complexity": "simple",
                "exit_code": 0,
                "ts": time.time(),
            })
        self.assertEqual(len(queen._task_history), 20)  # MAX_HISTORY cap


# --- Lead Reuse Tests ---

class TestLeadReuse(unittest.IsolatedAsyncioTestCase):
    """Test lead pool reuse logic."""

    async def _make_queen(self):
        queen = QueenAgent()
        queen._core = AsyncMock()
        queen.llm = MagicMock()
        queen.ask = AsyncMock(return_value='{"complexity": "complex"}')
        return queen

    async def test_acquire_reuses_idle_lead(self):
        queen = await self._make_queen()
        queen._idle_leads = [(42, time.time())]
        queen._core.get_process_info = AsyncMock(
            return_value=_mock_process_info(42, state=1)
        )
        ctx = _mock_ctx()

        pid = await queen._acquire_lead(ctx)
        self.assertEqual(pid, 42)
        self.assertEqual(len(queen._idle_leads), 0)
        ctx.spawn.assert_not_called()

    async def test_acquire_skips_dead_lead(self):
        queen = await self._make_queen()
        queen._idle_leads = [(42, time.time()), (43, time.time())]
        queen._core.get_process_info = AsyncMock(side_effect=[
            _mock_process_info(42, state=5),  # zombie
            _mock_process_info(43, state=1),  # running
        ])
        ctx = _mock_ctx()

        pid = await queen._acquire_lead(ctx)
        self.assertEqual(pid, 43)
        self.assertEqual(len(queen._idle_leads), 0)

    async def test_acquire_spawns_when_pool_empty(self):
        queen = await self._make_queen()
        ctx = _mock_ctx()
        ctx.spawn = AsyncMock(return_value=99)

        pid = await queen._acquire_lead(ctx)
        self.assertEqual(pid, 99)
        ctx.spawn.assert_called_once()

    async def test_acquire_spawns_when_all_dead(self):
        queen = await self._make_queen()
        queen._idle_leads = [(42, time.time())]
        queen._core.get_process_info = AsyncMock(
            side_effect=RuntimeError("not found")
        )
        ctx = _mock_ctx()
        ctx.spawn = AsyncMock(return_value=99)

        pid = await queen._acquire_lead(ctx)
        self.assertEqual(pid, 99)

    async def test_release_adds_to_pool(self):
        queen = await self._make_queen()
        await queen._release_lead(42)
        self.assertEqual(len(queen._idle_leads), 1)
        self.assertEqual(queen._idle_leads[0][0], 42)

    async def test_complex_task_releases_lead_on_success(self):
        queen = await self._make_queen()
        queen._core.get_artifact = AsyncMock(side_effect=RuntimeError("not found"))
        ctx = _mock_ctx()
        ctx.spawn = AsyncMock(return_value=50)

        task = _mock_task("research and analyze the impact of AI")
        await queen.handle_task(task, ctx)

        # Lead should be in idle pool after success.
        self.assertEqual(len(queen._idle_leads), 1)
        self.assertEqual(queen._idle_leads[0][0], 50)

    async def test_complex_task_kills_lead_on_failure(self):
        queen = await self._make_queen()
        queen._core.get_artifact = AsyncMock(side_effect=RuntimeError("not found"))
        ctx = _mock_ctx()
        ctx.spawn = AsyncMock(return_value=50)
        ctx.execute_on = AsyncMock(side_effect=RuntimeError("lead crashed"))

        task = _mock_task("research and analyze the impact of AI")
        result = await queen.handle_task(task, ctx)

        # Lead should be killed, not pooled.
        self.assertEqual(len(queen._idle_leads), 0)
        ctx.kill.assert_called_with(50)
        self.assertEqual(result.exit_code, 1)


# --- Lead Reaper Tests ---

class TestLeadReaper(unittest.IsolatedAsyncioTestCase):
    """Test background lead reaper."""

    async def test_reaper_kills_expired_leads(self):
        queen = QueenAgent()
        queen._core = AsyncMock()
        queen._core.kill_child = AsyncMock()

        # Add leads: one fresh, one expired.
        now = time.time()
        queen._idle_leads = [
            (10, now - 400),  # expired (> 300s)
            (11, now - 10),   # fresh
        ]

        # Run one reaper cycle manually.
        now_val = time.time()
        still_idle = []
        for pid, idle_since in queen._idle_leads:
            if now_val - idle_since > 300:
                try:
                    await queen._core.kill_child(pid)
                except Exception:
                    pass
            else:
                still_idle.append((pid, idle_since))
        queen._idle_leads = still_idle

        queen._core.kill_child.assert_called_once_with(10)
        self.assertEqual(len(queen._idle_leads), 1)
        self.assertEqual(queen._idle_leads[0][0], 11)


# --- Maid Integration Tests ---

class TestMaidIntegration(unittest.IsolatedAsyncioTestCase):
    """Test Queen-Maid health check integration."""

    async def test_health_check_returns_anomalies(self):
        queen = QueenAgent()
        report = {"anomalies": ["2 zombie(s): PID 5 (stuck-worker)"], "total": 5}
        artifact = MagicMock()
        artifact.content = json.dumps(report).encode("utf-8")
        queen._core = AsyncMock()
        queen._core.get_artifact = AsyncMock(return_value=artifact)

        warning = await queen._check_maid_health()
        self.assertIn("zombie", warning)

    async def test_health_check_no_anomalies(self):
        queen = QueenAgent()
        report = {"anomalies": [], "total": 3}
        artifact = MagicMock()
        artifact.content = json.dumps(report).encode("utf-8")
        queen._core = AsyncMock()
        queen._core.get_artifact = AsyncMock(return_value=artifact)

        warning = await queen._check_maid_health()
        self.assertEqual(warning, "")

    async def test_health_check_no_artifact(self):
        queen = QueenAgent()
        queen._core = AsyncMock()
        queen._core.get_artifact = AsyncMock(
            side_effect=RuntimeError("not found")
        )

        warning = await queen._check_maid_health()
        self.assertEqual(warning, "")

    async def test_health_check_no_core(self):
        queen = QueenAgent()
        queen._core = None

        warning = await queen._check_maid_health()
        self.assertEqual(warning, "")


# --- Shutdown Tests ---

class TestQueenShutdown(unittest.IsolatedAsyncioTestCase):
    """Test shutdown kills idle leads and cancels reaper."""

    async def test_shutdown_kills_idle_leads(self):
        queen = QueenAgent()
        queen._core = AsyncMock()
        queen._core.kill_child = AsyncMock()
        queen._idle_leads = [(10, time.time()), (11, time.time())]
        queen._reaper_task = None

        await queen.on_shutdown("normal")

        self.assertEqual(queen._core.kill_child.call_count, 2)
        self.assertEqual(len(queen._idle_leads), 0)

    async def test_shutdown_cancels_reaper(self):
        queen = QueenAgent()
        queen._core = AsyncMock()

        # Create a real background task to cancel.
        async def fake_reaper():
            while True:
                await asyncio.sleep(100)

        queen._reaper_task = asyncio.create_task(fake_reaper())
        queen._idle_leads = []

        await queen.on_shutdown("normal")
        self.assertTrue(queen._reaper_task.done())


# --- Init Tests ---

class TestQueenInit(unittest.IsolatedAsyncioTestCase):
    """Test on_init spawns maid and reaper."""

    async def test_on_init_starts_reaper(self):
        queen = QueenAgent()
        queen._core = AsyncMock()
        queen._core.spawn_child = AsyncMock(return_value=3)
        queen.llm = MagicMock()
        config = MagicMock()
        config.model = "sonnet"
        config.system_prompt = "test"

        # Mock LLMAgent.on_init to avoid real API key check.
        with patch.object(type(queen).__bases__[0], "on_init", new_callable=AsyncMock):
            await queen.on_init(config)

        self.assertIsNotNone(queen._reaper_task)
        self.assertFalse(queen._reaper_task.done())

        # Cleanup.
        queen._reaper_task.cancel()
        try:
            await queen._reaper_task
        except asyncio.CancelledError:
            pass


if __name__ == "__main__":
    unittest.main()
