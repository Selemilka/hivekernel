"""Unit tests for QueenAgent -- mailbox-first pattern.

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
    _ARCHITECT_KEYWORDS,
    SIMILARITY_THRESHOLD,
)
from hivekernel_sdk.types import Message, MessageAck


# --- Helper factories ---

def _mock_ctx():
    """Create a mock SyscallContext (only used for progress reporting now)."""
    ctx = AsyncMock()
    ctx.log = AsyncMock()
    ctx.report_progress = AsyncMock()
    # Old methods kept for backward compat in tests that still use ctx.spawn:
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


def _mock_core():
    """Create a mock CoreClient with standard returns."""
    core = AsyncMock()
    core.log = AsyncMock()
    core.spawn_child = AsyncMock(return_value=10)
    core.kill_child = AsyncMock()
    core.execute_task = AsyncMock(return_value={
        "exit_code": 0,
        "output": "test result",
        "artifacts": {},
        "metadata": {},
    })
    core.store_artifact = AsyncMock()
    core.get_artifact = AsyncMock(side_effect=RuntimeError("not found"))
    core.get_process_info = AsyncMock(return_value=_mock_process_info(10, state=1))
    core.send_message = AsyncMock(return_value="msg-1")
    core.list_siblings = AsyncMock(return_value=[])
    core.wait_child = AsyncMock(return_value={"pid": 10, "exit_code": 0, "output": ""})
    return core


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


def _mock_message(description="test task", from_pid=5, reply_to="req-123", trace_id=""):
    """Create a mock IPC message."""
    payload = {"task": description}
    if trace_id:
        payload["trace_id"] = trace_id
    return Message(
        message_id="msg-001",
        from_pid=from_pid,
        from_name="caller",
        type="task_request",
        payload=json.dumps(payload).encode("utf-8"),
        reply_to=reply_to,
    )


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
        queen._core = _mock_core()
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
        queen._core = _mock_core()
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

    async def test_history_recorded_after_handle_task(self):
        queen = await self._make_queen()
        ctx = _mock_ctx()
        task = _mock_task("summarize this article")

        await queen.handle_task(task, ctx)

        self.assertEqual(len(queen._task_history), 1)
        entry = queen._task_history[0]
        self.assertEqual(entry["task"], "summarize this article")
        self.assertEqual(entry["complexity"], "simple")

    async def test_history_recorded_after_ipc_task(self):
        queen = await self._make_queen()
        msg = _mock_message("summarize this article")

        await queen._process_message_task(msg)

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
        queen._core = _mock_core()
        queen.llm = MagicMock()
        queen.ask = AsyncMock(return_value='{"complexity": "complex"}')
        return queen

    async def test_acquire_reuses_running_lead(self):
        queen = await self._make_queen()
        queen._idle_leads = [(42, time.time())]
        queen._core.get_process_info = AsyncMock(
            return_value=_mock_process_info(42, state=1)
        )

        pid = await queen._acquire_lead()
        self.assertEqual(pid, 42)
        self.assertEqual(len(queen._idle_leads), 0)
        queen._core.spawn_child.assert_not_called()

    async def test_acquire_reuses_idle_state_lead(self):
        """Lead in IDLE state (0) should be reused -- this is the common case."""
        queen = await self._make_queen()
        queen._idle_leads = [(42, time.time())]
        queen._core.get_process_info = AsyncMock(
            return_value=_mock_process_info(42, state=0)  # IDLE
        )

        pid = await queen._acquire_lead()
        self.assertEqual(pid, 42)
        self.assertEqual(len(queen._idle_leads), 0)
        queen._core.spawn_child.assert_not_called()

    async def test_acquire_skips_dead_lead(self):
        queen = await self._make_queen()
        queen._idle_leads = [(42, time.time()), (43, time.time())]
        queen._core.get_process_info = AsyncMock(side_effect=[
            _mock_process_info(42, state=5),  # zombie
            _mock_process_info(43, state=1),  # running
        ])

        pid = await queen._acquire_lead()
        self.assertEqual(pid, 43)
        self.assertEqual(len(queen._idle_leads), 0)

    async def test_acquire_spawns_when_pool_empty(self):
        queen = await self._make_queen()
        queen._core.spawn_child = AsyncMock(return_value=99)

        pid = await queen._acquire_lead()
        self.assertEqual(pid, 99)
        queen._core.spawn_child.assert_called_once()

    async def test_acquire_spawns_when_all_dead(self):
        queen = await self._make_queen()
        queen._idle_leads = [(42, time.time())]
        queen._core.get_process_info = AsyncMock(
            side_effect=RuntimeError("not found")
        )
        queen._core.spawn_child = AsyncMock(return_value=99)

        pid = await queen._acquire_lead()
        self.assertEqual(pid, 99)

    async def test_acquire_with_ctx_spawns_via_ctx(self):
        """When ctx is provided, _acquire_lead should use ctx.spawn."""
        queen = await self._make_queen()
        ctx = _mock_ctx()
        ctx.spawn = AsyncMock(return_value=77)

        pid = await queen._acquire_lead(ctx=ctx)
        self.assertEqual(pid, 77)
        ctx.spawn.assert_called_once()
        queen._core.spawn_child.assert_not_called()

    async def test_release_adds_to_pool(self):
        queen = await self._make_queen()
        await queen._release_lead(42)
        self.assertEqual(len(queen._idle_leads), 1)
        self.assertEqual(queen._idle_leads[0][0], 42)

    async def test_complex_task_releases_lead_on_success(self):
        queen = await self._make_queen()
        queen._core.spawn_child = AsyncMock(return_value=50)
        ctx = _mock_ctx()
        task = _mock_task("research and analyze the impact of AI")

        await queen.handle_task(task, ctx)

        # Lead should be in idle pool after success.
        self.assertEqual(len(queen._idle_leads), 1)
        self.assertEqual(queen._idle_leads[0][0], 50)

    async def test_complex_task_kills_lead_on_failure(self):
        queen = await self._make_queen()
        queen._core.spawn_child = AsyncMock(return_value=50)
        queen._core.execute_task = AsyncMock(side_effect=RuntimeError("lead crashed"))
        ctx = _mock_ctx()
        task = _mock_task("research and analyze the impact of AI")

        result = await queen.handle_task(task, ctx)

        # Lead should be killed, not pooled.
        self.assertEqual(len(queen._idle_leads), 0)
        queen._core.kill_child.assert_called_with(50)
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


# --- Architect Routing Tests ---

class TestArchitectRouting(unittest.IsolatedAsyncioTestCase):
    """Test architect keyword detection and routing."""

    async def _make_queen(self):
        queen = QueenAgent()
        queen._core = _mock_core()
        queen.llm = MagicMock()
        queen.ask = AsyncMock(return_value='{"complexity": "complex"}')
        return queen

    async def test_architect_keyword_triggers(self):
        queen = await self._make_queen()
        result = await queen._assess_complexity("architect a microservices system")
        self.assertEqual(result, "architect")

    async def test_blueprint_keyword_triggers(self):
        queen = await self._make_queen()
        result = await queen._assess_complexity("create a blueprint for the new API")
        self.assertEqual(result, "architect")

    async def test_infrastructure_keyword_triggers(self):
        queen = await self._make_queen()
        result = await queen._assess_complexity("infrastructure planning for cloud")
        self.assertEqual(result, "architect")

    async def test_roadmap_keyword_triggers(self):
        queen = await self._make_queen()
        result = await queen._assess_complexity("roadmap for Q3 features")
        self.assertEqual(result, "architect")

    async def test_framework_keyword_triggers(self):
        queen = await self._make_queen()
        result = await queen._assess_complexity("design a framework for testing")
        self.assertEqual(result, "architect")

    async def test_simple_task_not_architect(self):
        queen = await self._make_queen()
        result = await queen._assess_complexity("explain what an API is")
        self.assertEqual(result, "simple")

    async def test_complex_task_not_architect(self):
        queen = await self._make_queen()
        result = await queen._assess_complexity("research and analyze AI impact on economy")
        self.assertEqual(result, "complex")

    async def test_architect_from_history(self):
        queen = await self._make_queen()
        queen._task_history.append({
            "task": "architect a payment system",
            "complexity": "architect",
            "exit_code": 0, "ts": 0,
        })
        result = await queen._assess_complexity("architect a payment gateway")
        self.assertEqual(result, "architect")


# --- Core Handle Architect Tests ---

class TestCoreHandleArchitect(unittest.IsolatedAsyncioTestCase):
    """Test _core_handle_architect flow."""

    async def _make_queen(self):
        queen = QueenAgent()
        queen._core = _mock_core()
        queen.llm = MagicMock()
        queen.ask = AsyncMock(return_value='{"complexity": "architect"}')
        return queen

    async def test_architect_flow_spawn_plan_execute(self):
        queen = await self._make_queen()

        spawn_pids = [101, 102]  # architect, then lead
        spawn_idx = [0]

        async def mock_spawn(**kwargs):
            pid = spawn_pids[spawn_idx[0]]
            spawn_idx[0] += 1
            return pid

        queen._core.spawn_child = mock_spawn

        execute_results = [
            # Architect result (plan)
            {
                "output": '{"groups": [{"name": "g1", "subtasks": ["do stuff"]}]}',
                "exit_code": 0, "metadata": {}, "artifacts": {},
            },
            # Lead result
            {
                "output": "Final result", "exit_code": 0,
                "metadata": {"groups_count": "1"}, "artifacts": {},
            },
        ]
        exec_idx = [0]

        async def mock_execute(target_pid, description, params, **kwargs):
            result = execute_results[exec_idx[0]]
            exec_idx[0] += 1
            return result

        queen._core.execute_task = mock_execute

        result = await queen._core_handle_architect("architect a new system")

        self.assertEqual(result["exit_code"], 0)
        self.assertEqual(result["metadata"]["strategy"], "architect")
        self.assertEqual(result["metadata"]["architect_pid"], "101")

    async def test_architect_failure_falls_back_to_complex(self):
        queen = await self._make_queen()

        spawn_count = [0]

        async def mock_spawn(**kwargs):
            spawn_count[0] += 1
            return 100 + spawn_count[0]

        queen._core.spawn_child = mock_spawn

        call_count = [0]

        async def mock_execute(target_pid, description, params, **kwargs):
            call_count[0] += 1
            if call_count[0] == 1:
                raise RuntimeError("architect crashed")
            # Fallback complex path: lead execution
            return {
                "output": "Fallback result", "exit_code": 0,
                "metadata": {}, "artifacts": {},
            }

        queen._core.execute_task = mock_execute

        result = await queen._core_handle_architect("architect a system")

        # Should still succeed via complex fallback.
        self.assertEqual(result["exit_code"], 0)
        # Spawned: architect (failed) + lead (fallback complex)
        self.assertGreaterEqual(spawn_count[0], 2)

    async def test_architect_lead_passes_plan(self):
        """Verify lead receives the plan in params."""
        queen = await self._make_queen()

        async def mock_spawn(**kwargs):
            return 42

        queen._core.spawn_child = mock_spawn

        execute_params_log = []

        async def mock_execute(target_pid, description, params, **kwargs):
            execute_params_log.append(params)
            return {"output": "result", "exit_code": 0, "metadata": {}, "artifacts": {}}

        queen._core.execute_task = mock_execute

        await queen._core_handle_architect("architect a system")

        # Second execute_task (to lead) should have "plan" param.
        self.assertGreaterEqual(len(execute_params_log), 2)
        lead_params = execute_params_log[1]
        self.assertIn("plan", lead_params)


# --- Core Handle Simple Tests ---

class TestCoreHandleSimple(unittest.IsolatedAsyncioTestCase):
    """Test _core_handle_simple flow."""

    async def test_simple_spawns_worker_and_executes(self):
        queen = QueenAgent()
        queen._core = _mock_core()
        queen._core.spawn_child = AsyncMock(return_value=20)
        queen._core.execute_task = AsyncMock(return_value={
            "exit_code": 0, "output": "done", "artifacts": {}, "metadata": {},
        })

        result = await queen._core_handle_simple("summarize this")

        queen._core.spawn_child.assert_called_once()
        queen._core.execute_task.assert_called_once()
        self.assertEqual(result["exit_code"], 0)
        self.assertEqual(result["metadata"]["strategy"], "simple")

    async def test_simple_kills_worker_on_failure(self):
        queen = QueenAgent()
        queen._core = _mock_core()
        queen._core.spawn_child = AsyncMock(return_value=20)
        queen._core.execute_task = AsyncMock(side_effect=RuntimeError("worker died"))

        result = await queen._core_handle_simple("do stuff")

        self.assertEqual(result["exit_code"], 1)
        queen._core.kill_child.assert_called_with(20)


# --- Core Handle Complex Tests ---

class TestCoreHandleComplex(unittest.IsolatedAsyncioTestCase):
    """Test _core_handle_complex flow."""

    async def test_complex_acquires_lead_and_releases(self):
        queen = QueenAgent()
        queen._core = _mock_core()
        queen._core.spawn_child = AsyncMock(return_value=30)
        queen._core.execute_task = AsyncMock(return_value={
            "exit_code": 0, "output": "synthesized", "artifacts": {}, "metadata": {},
        })

        result = await queen._core_handle_complex("research and analyze AI")

        self.assertEqual(result["exit_code"], 0)
        self.assertEqual(result["metadata"]["strategy"], "complex")
        # Lead should be in idle pool.
        self.assertEqual(len(queen._idle_leads), 1)
        self.assertEqual(queen._idle_leads[0][0], 30)

    async def test_complex_kills_lead_on_failure(self):
        queen = QueenAgent()
        queen._core = _mock_core()
        queen._core.spawn_child = AsyncMock(return_value=30)
        queen._core.execute_task = AsyncMock(side_effect=RuntimeError("crashed"))

        result = await queen._core_handle_complex("research stuff")

        self.assertEqual(result["exit_code"], 1)
        queen._core.kill_child.assert_called_with(30)
        self.assertEqual(len(queen._idle_leads), 0)


# --- handle_task (Execute stream thin wrapper) Tests ---

class TestHandleTask(unittest.IsolatedAsyncioTestCase):
    """Test handle_task delegates to _core_handle_* methods."""

    async def _make_queen(self):
        queen = QueenAgent()
        queen._core = _mock_core()
        queen.llm = MagicMock()
        queen.ask = AsyncMock(return_value='{"complexity": "complex"}')
        return queen

    async def test_handle_task_simple(self):
        queen = await self._make_queen()
        ctx = _mock_ctx()
        task = _mock_task("summarize this article")

        result = await queen.handle_task(task, ctx)

        self.assertEqual(result.exit_code, 0)
        # ctx.report_progress should still be called (progress for stream callers).
        ctx.report_progress.assert_called()
        # Core methods should be used.
        queen._core.spawn_child.assert_called()
        queen._core.execute_task.assert_called()

    async def test_handle_task_complex(self):
        queen = await self._make_queen()
        queen._core.spawn_child = AsyncMock(return_value=50)
        ctx = _mock_ctx()
        task = _mock_task("research and analyze the impact of AI")

        result = await queen.handle_task(task, ctx)

        self.assertEqual(result.exit_code, 0)
        self.assertIn("complex", result.metadata.get("strategy", ""))

    async def test_handle_task_records_history(self):
        queen = await self._make_queen()
        ctx = _mock_ctx()
        task = _mock_task("summarize this text")

        await queen.handle_task(task, ctx)

        self.assertEqual(len(queen._task_history), 1)

    async def test_handle_task_stores_artifact(self):
        queen = await self._make_queen()
        ctx = _mock_ctx()
        task = _mock_task("summarize this text")

        await queen.handle_task(task, ctx)

        queen._core.store_artifact.assert_called_once()

    async def test_handle_task_empty_description(self):
        queen = await self._make_queen()
        ctx = _mock_ctx()
        task = _mock_task("", params={"task": ""})

        result = await queen.handle_task(task, ctx)

        self.assertEqual(result.exit_code, 1)
        self.assertIn("Empty", result.output)


# --- handle_message (IPC path) Tests ---

class TestHandleMessage(unittest.IsolatedAsyncioTestCase):
    """Test unified handle_message path."""

    async def _make_queen(self):
        queen = QueenAgent()
        queen._core = _mock_core()
        queen.llm = MagicMock()
        queen.ask = AsyncMock(return_value='{"complexity": "complex"}')
        return queen

    async def test_ipc_simple_task(self):
        queen = await self._make_queen()
        msg = _mock_message("summarize this article")

        await queen._process_message_task(msg)

        # Should have spawned worker via core.
        queen._core.spawn_child.assert_called()
        queen._core.execute_task.assert_called()
        # Should send reply.
        queen._core.send_message.assert_called()
        call_kwargs = queen._core.send_message.call_args
        self.assertEqual(call_kwargs.kwargs.get("to_pid") or call_kwargs[1].get("to_pid", 0), 5)

    async def test_ipc_complex_task(self):
        queen = await self._make_queen()
        queen._core.spawn_child = AsyncMock(return_value=50)
        msg = _mock_message("research and analyze the impact of AI")

        await queen._process_message_task(msg)

        queen._core.spawn_child.assert_called()
        queen._core.execute_task.assert_called()

    async def test_ipc_architect_task(self):
        """Architect path should work through IPC (was missing before)."""
        queen = await self._make_queen()

        spawn_count = [0]

        async def mock_spawn(**kwargs):
            spawn_count[0] += 1
            return 100 + spawn_count[0]

        queen._core.spawn_child = mock_spawn

        exec_count = [0]

        async def mock_exec(target_pid, description, params, **kwargs):
            exec_count[0] += 1
            return {"output": "plan" if exec_count[0] == 1 else "result",
                    "exit_code": 0, "metadata": {}, "artifacts": {}}

        queen._core.execute_task = mock_exec

        msg = _mock_message("architect a microservices system")
        await queen._process_message_task(msg)

        # Should have spawned architect + lead.
        self.assertGreaterEqual(spawn_count[0], 2)

    async def test_ipc_records_history(self):
        queen = await self._make_queen()
        msg = _mock_message("summarize this article")

        await queen._process_message_task(msg)

        self.assertEqual(len(queen._task_history), 1)

    async def test_ipc_stores_artifact(self):
        queen = await self._make_queen()
        msg = _mock_message("summarize this article")

        await queen._process_message_task(msg)

        queen._core.store_artifact.assert_called()

    async def test_ipc_lead_reuse(self):
        """IPC path should reuse leads (was killing them before)."""
        queen = await self._make_queen()
        queen._core.spawn_child = AsyncMock(return_value=50)

        # First complex task.
        msg1 = _mock_message("research and analyze topic A")
        await queen._process_message_task(msg1)

        # Lead should be in pool.
        self.assertEqual(len(queen._idle_leads), 1)
        self.assertEqual(queen._idle_leads[0][0], 50)

    async def test_ipc_empty_task(self):
        queen = await self._make_queen()
        msg = _mock_message("")

        await queen._process_message_task(msg)

        # Should send error reply.
        queen._core.send_message.assert_called()

    async def test_ipc_sends_reply_with_correct_reply_to(self):
        queen = await self._make_queen()
        msg = _mock_message("summarize this", reply_to="original-msg-id")

        await queen._process_message_task(msg)

        call_kwargs = queen._core.send_message.call_args
        # reply_to should be the original message's reply_to for correlation.
        self.assertEqual(
            call_kwargs.kwargs.get("reply_to") or call_kwargs[1].get("reply_to"),
            "original-msg-id",
        )

    async def test_handle_message_queues_task_request(self):
        queen = await self._make_queen()
        msg = _mock_message("test task")

        ack = await queen.handle_message(msg)

        self.assertEqual(ack.status, MessageAck.ACK_QUEUED)

    async def test_handle_message_accepts_non_task(self):
        queen = await self._make_queen()
        msg = Message(
            message_id="m1", from_pid=5, type="ping", payload=b"",
        )

        ack = await queen.handle_message(msg)

        self.assertEqual(ack.status, MessageAck.ACK_ACCEPTED)


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
