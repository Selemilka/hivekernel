"""Unit tests for OrchestratorAgent and WorkerAgent (Phase 4).

Run: python sdk/python/tests/test_orchestrator.py -v
"""

import asyncio
import json
import os
import sys
import unittest
from unittest.mock import AsyncMock, MagicMock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from hivekernel_sdk.orchestrator import OrchestratorAgent
from hivekernel_sdk.worker import WorkerAgent


# --- ParseGroups tests (synchronous, no async needed) ---


class TestParseGroups(unittest.TestCase):
    """Test OrchestratorAgent._parse_groups with various inputs."""

    def setUp(self):
        self.orch = OrchestratorAgent()

    def test_valid_groups_json(self):
        raw = json.dumps({
            "groups": [
                {"name": "data", "subtasks": ["gather data", "clean data"]},
                {"name": "analysis", "subtasks": ["analyze trends"]},
            ]
        })
        groups = self.orch._parse_groups(raw, max_workers=3)
        self.assertEqual(len(groups), 2)
        self.assertEqual(groups[0]["name"], "data")
        self.assertEqual(len(groups[0]["subtasks"]), 2)
        self.assertEqual(groups[1]["name"], "analysis")

    def test_flat_list_fallback(self):
        raw = json.dumps(["task A", "task B", "task C"])
        groups = self.orch._parse_groups(raw, max_workers=5)
        self.assertEqual(len(groups), 3)
        self.assertEqual(groups[0]["subtasks"], ["task A"])
        self.assertEqual(groups[2]["subtasks"], ["task C"])

    def test_newlines_fallback(self):
        raw = "1. First task\n2. Second task\n3. Third task"
        groups = self.orch._parse_groups(raw, max_workers=5)
        self.assertEqual(len(groups), 3)
        for g in groups:
            self.assertEqual(len(g["subtasks"]), 1)

    def test_markdown_fences(self):
        raw = '```json\n{"groups": [{"name": "g1", "subtasks": ["do stuff"]}]}\n```'
        groups = self.orch._parse_groups(raw, max_workers=3)
        self.assertEqual(len(groups), 1)
        self.assertEqual(groups[0]["subtasks"], ["do stuff"])

    def test_empty_input(self):
        groups = self.orch._parse_groups("", max_workers=3)
        self.assertEqual(groups, [])

    def test_single_text_line(self):
        groups = self.orch._parse_groups("just one task", max_workers=3)
        self.assertEqual(len(groups), 1)
        self.assertEqual(groups[0]["subtasks"], ["just one task"])

    def test_groups_with_empty_subtasks_skipped(self):
        raw = json.dumps({
            "groups": [
                {"name": "valid", "subtasks": ["do thing"]},
                {"name": "empty", "subtasks": []},
            ]
        })
        groups = self.orch._parse_groups(raw, max_workers=3)
        self.assertEqual(len(groups), 1)
        self.assertEqual(groups[0]["name"], "valid")

    def test_groups_missing_name_gets_default(self):
        raw = json.dumps({
            "groups": [
                {"subtasks": ["task A"]},
            ]
        })
        groups = self.orch._parse_groups(raw, max_workers=3)
        self.assertEqual(len(groups), 1)
        self.assertIn("group-", groups[0]["name"])

    def test_flat_list_caps_at_max_workers(self):
        raw = json.dumps(["t1", "t2", "t3", "t4", "t5"])
        groups = self.orch._parse_groups(raw, max_workers=2)
        self.assertEqual(len(groups), 2)


# --- CapGroups tests ---


class TestCapGroups(unittest.TestCase):
    """Test _cap_groups merging logic."""

    def test_no_cap_needed(self):
        groups = [
            {"name": "a", "subtasks": ["t1"]},
            {"name": "b", "subtasks": ["t2"]},
        ]
        result = OrchestratorAgent._cap_groups(groups, max_workers=3)
        self.assertEqual(len(result), 2)

    def test_cap_merges_excess_round_robin(self):
        groups = [
            {"name": "a", "subtasks": ["t1"]},
            {"name": "b", "subtasks": ["t2"]},
            {"name": "c", "subtasks": ["t3"]},
            {"name": "d", "subtasks": ["t4"]},
            {"name": "e", "subtasks": ["t5"]},
        ]
        result = OrchestratorAgent._cap_groups(groups, max_workers=2)
        self.assertEqual(len(result), 2)
        # c -> a, d -> b, e -> a (round-robin)
        self.assertEqual(result[0]["subtasks"], ["t1", "t3", "t5"])
        self.assertEqual(result[1]["subtasks"], ["t2", "t4"])

    def test_cap_exact_match(self):
        groups = [
            {"name": "a", "subtasks": ["t1"]},
            {"name": "b", "subtasks": ["t2"]},
        ]
        result = OrchestratorAgent._cap_groups(groups, max_workers=2)
        self.assertEqual(len(result), 2)

    def test_cap_does_not_mutate_original(self):
        groups = [
            {"name": "a", "subtasks": ["t1"]},
            {"name": "b", "subtasks": ["t2"]},
            {"name": "c", "subtasks": ["t3"]},
        ]
        OrchestratorAgent._cap_groups(groups, max_workers=2)
        # Original should be unchanged
        self.assertEqual(len(groups[0]["subtasks"]), 1)

    def test_cap_single_worker(self):
        groups = [
            {"name": "a", "subtasks": ["t1"]},
            {"name": "b", "subtasks": ["t2"]},
            {"name": "c", "subtasks": ["t3"]},
        ]
        result = OrchestratorAgent._cap_groups(groups, max_workers=1)
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]["subtasks"], ["t1", "t2", "t3"])


# --- DecomposeAndGroup tests (async) ---


class TestDecomposeAndGroup(unittest.IsolatedAsyncioTestCase):
    """Test _decompose_and_group with mocked LLM."""

    async def test_decompose_returns_groups(self):
        orch = OrchestratorAgent()
        orch.ask = AsyncMock(return_value=json.dumps({
            "groups": [
                {"name": "research", "subtasks": ["find papers", "read papers"]},
                {"name": "writing", "subtasks": ["write draft"]},
            ]
        }))

        groups = await orch._decompose_and_group("research and write", 3)
        self.assertEqual(len(groups), 2)
        self.assertEqual(groups[0]["name"], "research")
        self.assertEqual(len(groups[0]["subtasks"]), 2)
        orch.ask.assert_called_once()

    async def test_decompose_llm_returns_flat_list(self):
        orch = OrchestratorAgent()
        orch.ask = AsyncMock(return_value=json.dumps(["task A", "task B"]))

        groups = await orch._decompose_and_group("do things", 3)
        self.assertEqual(len(groups), 2)
        self.assertEqual(groups[0]["subtasks"], ["task A"])

    async def test_decompose_respects_max_workers(self):
        orch = OrchestratorAgent()
        orch.ask = AsyncMock(return_value=json.dumps({
            "groups": [
                {"name": "a", "subtasks": ["t1"]},
                {"name": "b", "subtasks": ["t2"]},
                {"name": "c", "subtasks": ["t3"]},
                {"name": "d", "subtasks": ["t4"]},
            ]
        }))

        groups = await orch._decompose_and_group("big task", 2)
        self.assertEqual(len(groups), 2)


# --- Full handle_task tests ---


class TestOrchestratorHandleTask(unittest.IsolatedAsyncioTestCase):
    """Test full handle_task flow."""

    async def _setup_orch(self, groups_json, synthesis="Final report."):
        """Create an OrchestratorAgent with mocked LLM."""
        orch = OrchestratorAgent()

        call_count = [0]

        async def mock_ask(prompt, **kwargs):
            call_count[0] += 1
            if call_count[0] == 1:
                return groups_json  # decomposition
            return synthesis  # synthesis

        orch.ask = mock_ask
        orch.llm = MagicMock()
        orch.llm.total_tokens = 100
        return orch

    def _make_task(self, description="big research"):
        task = MagicMock()
        task.params = {"task": description}
        task.description = description
        return task

    def _make_ctx(self):
        """Create a mock SyscallContext."""
        ctx = AsyncMock()
        spawn_counter = [0]

        async def mock_spawn(**kwargs):
            spawn_counter[0] += 1
            return 100 + spawn_counter[0]

        ctx.spawn = mock_spawn
        ctx.execute_on = AsyncMock(return_value=MagicMock(
            output="result", exit_code=0,
            metadata={"tokens_used": "50"}, artifacts={},
        ))
        ctx._spawn_counter = spawn_counter
        return ctx

    async def test_spawns_workers_as_role_worker(self):
        orch = await self._setup_orch(json.dumps({
            "groups": [
                {"name": "g1", "subtasks": ["sub1"]},
                {"name": "g2", "subtasks": ["sub2"]},
            ]
        }))

        spawn_calls = []

        async def mock_spawn(**kwargs):
            spawn_calls.append(kwargs)
            return 100 + len(spawn_calls)

        ctx = AsyncMock()
        ctx.spawn = mock_spawn
        ctx.execute_on = AsyncMock(return_value=MagicMock(
            output="result", exit_code=0,
            metadata={"tokens_used": "50"}, artifacts={},
        ))

        await orch.handle_task(self._make_task(), ctx)

        self.assertEqual(len(spawn_calls), 2)
        self.assertEqual(spawn_calls[0]["role"], "worker")
        self.assertEqual(spawn_calls[1]["role"], "worker")

    async def test_groups_use_separate_workers(self):
        """Each group gets its own worker; subtasks in same group share worker."""
        orch = await self._setup_orch(json.dumps({
            "groups": [
                {"name": "g1", "subtasks": ["sub1a", "sub1b"]},
                {"name": "g2", "subtasks": ["sub2a"]},
            ]
        }))

        execution_log = []
        spawn_counter = [0]

        async def mock_spawn(**kwargs):
            spawn_counter[0] += 1
            return 100 + spawn_counter[0]

        async def mock_execute_on(pid, description, params, **kwargs):
            execution_log.append((pid, params.get("subtask", description)))
            return MagicMock(
                output=f"result-{description}", exit_code=0,
                metadata={"tokens_used": "50"}, artifacts={},
            )

        ctx = AsyncMock()
        ctx.spawn = mock_spawn
        ctx.execute_on = mock_execute_on

        result = await orch.handle_task(self._make_task("multi-step"), ctx)

        # All 3 subtasks executed
        self.assertEqual(len(execution_log), 3)

        # Group 1 subtasks on same worker PID
        g1_entries = [(pid, s) for pid, s in execution_log if "sub1" in s]
        self.assertEqual(len(g1_entries), 2)
        self.assertEqual(g1_entries[0][0], g1_entries[1][0])

        # Group 2 on different worker
        g2_entries = [(pid, s) for pid, s in execution_log if "sub2" in s]
        self.assertNotEqual(g1_entries[0][0], g2_entries[0][0])

        self.assertEqual(result.exit_code, 0)

    async def test_workers_killed_after_completion(self):
        orch = await self._setup_orch(json.dumps({
            "groups": [
                {"name": "g1", "subtasks": ["sub1"]},
                {"name": "g2", "subtasks": ["sub2"]},
            ]
        }))

        spawned_pids = []

        async def mock_spawn(**kwargs):
            pid = 100 + len(spawned_pids) + 1
            spawned_pids.append(pid)
            return pid

        killed_pids = []

        async def mock_kill(pid):
            killed_pids.append(pid)

        ctx = AsyncMock()
        ctx.spawn = mock_spawn
        ctx.execute_on = AsyncMock(return_value=MagicMock(
            output="done", exit_code=0,
            metadata={"tokens_used": "50"}, artifacts={},
        ))
        ctx.kill = mock_kill

        await orch.handle_task(self._make_task(), ctx)

        self.assertEqual(sorted(spawned_pids), sorted(killed_pids))

    async def test_metadata_includes_groups_info(self):
        orch = await self._setup_orch(json.dumps({
            "groups": [
                {"name": "data", "subtasks": ["get data", "clean data"]},
                {"name": "report", "subtasks": ["write report"]},
            ]
        }))

        ctx = self._make_ctx()
        result = await orch.handle_task(self._make_task("data report"), ctx)

        self.assertEqual(result.metadata["groups_count"], "2")
        self.assertEqual(result.metadata["subtasks_count"], "3")
        # "data" group has 2 subtasks -> 1 worker reused
        self.assertEqual(result.metadata["workers_reused"], "1")

    async def test_worker_error_captured_in_results(self):
        orch = await self._setup_orch(json.dumps({
            "groups": [{"name": "g1", "subtasks": ["sub1"]}]
        }))

        ctx = self._make_ctx()
        ctx.execute_on = AsyncMock(side_effect=RuntimeError("worker crashed"))

        result = await orch.handle_task(self._make_task("fail task"), ctx)
        # Still succeeds overall (synthesis runs with error results)
        self.assertEqual(result.exit_code, 0)

    async def test_empty_decomposition_returns_error(self):
        orch = await self._setup_orch("")  # LLM returns empty

        ctx = self._make_ctx()
        result = await orch.handle_task(self._make_task("bad task"), ctx)

        self.assertEqual(result.exit_code, 1)
        self.assertIn("Failed to decompose", result.output)

    async def test_single_group_single_subtask(self):
        """Simplest case: one group with one subtask."""
        orch = await self._setup_orch(json.dumps({
            "groups": [{"name": "solo", "subtasks": ["do the thing"]}]
        }))

        ctx = self._make_ctx()
        result = await orch.handle_task(self._make_task("solo task"), ctx)

        self.assertEqual(result.exit_code, 0)
        self.assertEqual(result.metadata["groups_count"], "1")
        self.assertEqual(result.metadata["workers_reused"], "0")


# --- Worker tests ---


class TestWorkerMemory(unittest.IsolatedAsyncioTestCase):
    """Test WorkerAgent accumulated context."""

    async def _make_worker(self):
        worker = WorkerAgent()
        worker.llm = MagicMock()
        worker.llm.total_tokens = 50
        return worker

    def _make_task(self, subtask="do thing"):
        task = MagicMock()
        task.params = {"subtask": subtask}
        task.description = subtask
        return task

    async def test_first_task_no_context(self):
        worker = await self._make_worker()
        prompts = []

        async def mock_ask(prompt, **kwargs):
            prompts.append(prompt)
            return "result"

        worker.ask = mock_ask
        ctx = AsyncMock()

        await worker.handle_task(self._make_task("do thing"), ctx)

        self.assertEqual(len(worker._task_memory), 1)
        # First task should NOT have "Previous task" context
        self.assertNotIn("Previous task", prompts[0])

    async def test_second_task_has_context(self):
        worker = await self._make_worker()
        prompts = []

        async def mock_ask(prompt, **kwargs):
            prompts.append(prompt)
            return "result"

        worker.ask = mock_ask
        ctx = AsyncMock()

        await worker.handle_task(self._make_task("find data"), ctx)
        await worker.handle_task(self._make_task("analyze data"), ctx)

        self.assertEqual(len(worker._task_memory), 2)
        # Second prompt should include context from first
        self.assertIn("Previous task 1", prompts[1])
        self.assertIn("find data", prompts[1])

    async def test_third_task_has_both_previous(self):
        worker = await self._make_worker()
        prompts = []

        async def mock_ask(prompt, **kwargs):
            prompts.append(prompt)
            return f"result-{len(prompts)}"

        worker.ask = mock_ask
        ctx = AsyncMock()

        await worker.handle_task(self._make_task("step 1"), ctx)
        await worker.handle_task(self._make_task("step 2"), ctx)
        await worker.handle_task(self._make_task("step 3"), ctx)

        self.assertIn("Previous task 1", prompts[2])
        self.assertIn("Previous task 2", prompts[2])
        self.assertIn("step 1", prompts[2])
        self.assertIn("step 2", prompts[2])

    async def test_memory_accumulates(self):
        worker = await self._make_worker()
        worker.ask = AsyncMock(return_value="result")
        ctx = AsyncMock()

        for i in range(5):
            await worker.handle_task(self._make_task(f"task-{i}"), ctx)

        self.assertEqual(len(worker._task_memory), 5)

    async def test_metadata_has_tasks_completed(self):
        worker = await self._make_worker()
        worker.ask = AsyncMock(return_value="result")
        ctx = AsyncMock()

        result1 = await worker.handle_task(self._make_task("first"), ctx)
        self.assertEqual(result1.metadata["tasks_completed"], "1")

        result2 = await worker.handle_task(self._make_task("second"), ctx)
        self.assertEqual(result2.metadata["tasks_completed"], "2")

    async def test_output_from_llm_returned(self):
        worker = await self._make_worker()
        worker.ask = AsyncMock(return_value="LLM says hello")
        ctx = AsyncMock()

        result = await worker.handle_task(self._make_task("greet"), ctx)

        self.assertEqual(result.exit_code, 0)
        self.assertEqual(result.output, "LLM says hello")

    async def test_memory_stores_task_and_output(self):
        worker = await self._make_worker()
        worker.ask = AsyncMock(return_value="computed result")
        ctx = AsyncMock()

        await worker.handle_task(self._make_task("compute X"), ctx)

        self.assertEqual(len(worker._task_memory), 1)
        self.assertEqual(worker._task_memory[0]["task"], "compute X")
        self.assertEqual(worker._task_memory[0]["output"], "computed result")


if __name__ == "__main__":
    unittest.main()
