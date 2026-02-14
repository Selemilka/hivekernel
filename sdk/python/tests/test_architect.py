"""Unit tests for ArchitectAgent (Phase 5).

Run: python sdk/python/tests/test_architect.py -v
"""

import json
import os
import sys
import unittest
from unittest.mock import AsyncMock, MagicMock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from hivekernel_sdk.architect import ArchitectAgent


VALID_PLAN = json.dumps({
    "analysis": "Task requires data collection and analysis",
    "approach": "Two-phase: collect then analyze",
    "groups": [
        {"name": "collection", "subtasks": ["gather data A", "gather data B"],
         "rationale": "Related data sources"},
        {"name": "analysis", "subtasks": ["analyze trends"],
         "rationale": "Depends on collected data"},
    ],
    "max_workers": 2,
    "risks": "Data sources may be unavailable",
    "acceptance_criteria": "Report with trends produced",
})


class TestArchitectHandleTask(unittest.IsolatedAsyncioTestCase):
    """Test ArchitectAgent plan generation."""

    async def _make_architect(self):
        arch = ArchitectAgent()
        arch.llm = MagicMock()
        arch.llm.total_tokens = 200
        return arch

    def _make_task(self, description="design a system"):
        task = MagicMock()
        task.params = {"task": description, "max_workers": "3"}
        task.description = description
        return task

    async def test_produces_valid_plan(self):
        arch = await self._make_architect()
        arch.ask = AsyncMock(return_value=VALID_PLAN)
        ctx = AsyncMock()

        result = await arch.handle_task(self._make_task(), ctx)

        self.assertEqual(result.exit_code, 0)
        plan = json.loads(result.output)
        self.assertIn("groups", plan)
        self.assertEqual(len(plan["groups"]), 2)
        self.assertEqual(plan["groups"][0]["name"], "collection")

    async def test_stores_plan_artifact(self):
        arch = await self._make_architect()
        arch.ask = AsyncMock(return_value=VALID_PLAN)
        ctx = AsyncMock()

        await arch.handle_task(self._make_task(), ctx)

        ctx.store_artifact.assert_called_once()
        call_kwargs = ctx.store_artifact.call_args
        self.assertEqual(call_kwargs.kwargs["key"], "architect/plan")
        self.assertEqual(call_kwargs.kwargs["content_type"], "application/json")

    async def test_invalid_llm_output_uses_fallback(self):
        arch = await self._make_architect()
        arch.ask = AsyncMock(return_value="not valid json at all")
        ctx = AsyncMock()

        result = await arch.handle_task(
            self._make_task("build something"), ctx)

        self.assertEqual(result.exit_code, 0)
        plan = json.loads(result.output)
        # Fallback plan should have the original task as subtask.
        self.assertEqual(plan["groups"][0]["subtasks"], ["build something"])
        self.assertIn("fallback", plan["analysis"].lower())

    async def test_markdown_fences_stripped(self):
        arch = await self._make_architect()
        arch.ask = AsyncMock(return_value=f"```json\n{VALID_PLAN}\n```")
        ctx = AsyncMock()

        result = await arch.handle_task(self._make_task(), ctx)

        self.assertEqual(result.exit_code, 0)
        plan = json.loads(result.output)
        self.assertIn("groups", plan)

    async def test_plan_missing_groups_uses_fallback(self):
        arch = await self._make_architect()
        arch.ask = AsyncMock(return_value=json.dumps(
            {"analysis": "ok", "approach": "direct"}
        ))
        ctx = AsyncMock()

        result = await arch.handle_task(
            self._make_task("fix the bug"), ctx)

        plan = json.loads(result.output)
        # Fallback plan
        self.assertEqual(plan["groups"][0]["subtasks"], ["fix the bug"])

    async def test_plan_empty_groups_uses_fallback(self):
        arch = await self._make_architect()
        arch.ask = AsyncMock(return_value=json.dumps(
            {"groups": [], "analysis": "ok"}
        ))
        ctx = AsyncMock()

        result = await arch.handle_task(
            self._make_task("do thing"), ctx)

        plan = json.loads(result.output)
        self.assertEqual(plan["groups"][0]["subtasks"], ["do thing"])

    async def test_metadata_has_plan_size(self):
        arch = await self._make_architect()
        arch.ask = AsyncMock(return_value=VALID_PLAN)
        ctx = AsyncMock()

        result = await arch.handle_task(self._make_task(), ctx)

        self.assertIn("plan_size", result.metadata)
        self.assertGreater(int(result.metadata["plan_size"]), 0)

    async def test_artifact_store_failure_no_crash(self):
        arch = await self._make_architect()
        arch.ask = AsyncMock(return_value=VALID_PLAN)
        ctx = AsyncMock()
        ctx.store_artifact = AsyncMock(side_effect=RuntimeError("store failed"))

        result = await arch.handle_task(self._make_task(), ctx)
        # Should still succeed despite artifact failure.
        self.assertEqual(result.exit_code, 0)


if __name__ == "__main__":
    unittest.main()
