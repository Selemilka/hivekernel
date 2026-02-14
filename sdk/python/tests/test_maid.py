"""Unit tests for MaidAgent (maid.py).

Run: python sdk/python/tests/test_maid.py -v
"""

import asyncio
import json
import os
import sys
import unittest
from unittest.mock import AsyncMock, MagicMock, patch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from hivekernel_sdk.maid import MaidAgent, _STATE_ZOMBIE


def _make_process_info(pid, name, state=1, ppid=1, role=6, tokens=0):
    """Create a mock ProcessInfo proto response."""
    info = MagicMock()
    info.pid = pid
    info.ppid = ppid
    info.name = name
    info.state = state
    info.role = role
    info.tokens_consumed = tokens
    return info


class TestMaidScan(unittest.IsolatedAsyncioTestCase):
    """Test Maid's process scanning logic."""

    async def test_scan_finds_processes(self):
        agent = MaidAgent()
        agent._max_pid_scan = 5

        mock_core = AsyncMock()
        # PID 1 = king, PID 2 = queen, PID 3 = maid, PID 4/5 = not found
        async def get_info(pid):
            procs = {
                1: _make_process_info(1, "king", state=1, ppid=0, role=0),
                2: _make_process_info(2, "queen@vps1", state=1, ppid=1, role=1),
                3: _make_process_info(3, "maid@local", state=1, ppid=2, role=1),
            }
            if pid in procs:
                return procs[pid]
            raise RuntimeError(f"process {pid} not found")

        mock_core.get_process_info = AsyncMock(side_effect=get_info)
        agent._core = mock_core

        report = await agent._scan()
        self.assertEqual(report["total"], 3)
        self.assertEqual(report["zombie_count"], 0)
        self.assertEqual(report["anomalies"], [])

    async def test_scan_detects_zombies(self):
        agent = MaidAgent()
        agent._max_pid_scan = 4

        mock_core = AsyncMock()
        async def get_info(pid):
            procs = {
                1: _make_process_info(1, "king", state=1),
                2: _make_process_info(2, "queen@vps1", state=1),
                3: _make_process_info(3, "dead-worker", state=_STATE_ZOMBIE, ppid=2),
            }
            if pid in procs:
                return procs[pid]
            raise RuntimeError(f"process {pid} not found")

        mock_core.get_process_info = AsyncMock(side_effect=get_info)
        agent._core = mock_core

        report = await agent._scan()
        self.assertEqual(report["zombie_count"], 1)
        self.assertEqual(len(report["anomalies"]), 1)
        self.assertIn("PID 3", report["anomalies"][0])
        self.assertIn("dead-worker", report["anomalies"][0])

    async def test_scan_no_core_client(self):
        agent = MaidAgent()
        agent._core = None

        report = await agent._scan()
        self.assertEqual(report["total"], 0)
        self.assertIn("no core client", report["anomalies"])

    async def test_format_report(self):
        agent = MaidAgent()
        report = {
            "ts": 0,
            "total": 3,
            "zombie_count": 0,
            "zombies": [],
            "anomalies": [],
            "processes": [],
        }
        text = agent._format_report(report)
        self.assertIn("Total processes: 3", text)
        self.assertIn("No anomalies", text)

    async def test_format_report_with_zombies(self):
        agent = MaidAgent()
        report = {
            "ts": 0,
            "total": 3,
            "zombie_count": 1,
            "zombies": [{"pid": 5, "name": "stuck-worker", "state": 5}],
            "anomalies": ["1 zombie(s): PID 5 (stuck-worker)"],
            "processes": [],
        }
        text = agent._format_report(report)
        self.assertIn("Zombies: 1", text)
        self.assertIn("ZOMBIE: PID 5", text)

    async def test_handle_task_returns_report(self):
        """On-demand health check via execute_on."""
        agent = MaidAgent()
        agent._max_pid_scan = 3

        mock_core = AsyncMock()
        async def get_info(pid):
            if pid == 1:
                return _make_process_info(1, "king", state=1)
            raise RuntimeError("not found")

        mock_core.get_process_info = AsyncMock(side_effect=get_info)
        agent._core = mock_core

        # Mock task and ctx.
        task = MagicMock()
        task.description = "health-check"
        task.params = {}
        ctx = MagicMock()

        result = await agent.handle_task(task, ctx)
        self.assertEqual(result.exit_code, 0)
        self.assertIn("Total processes: 1", result.output)


class TestMaidLifecycle(unittest.IsolatedAsyncioTestCase):
    """Test Maid daemon lifecycle."""

    async def test_on_init_starts_loop(self):
        agent = MaidAgent()
        agent._core = AsyncMock()
        config = MagicMock()
        config.name = "maid@local"

        agent._pid = 3
        await agent.on_init(config)

        self.assertIsNotNone(agent._loop_task)
        self.assertFalse(agent._loop_task.done())

        # Clean up.
        agent._loop_task.cancel()
        try:
            await agent._loop_task
        except asyncio.CancelledError:
            pass

    async def test_on_shutdown_cancels_loop(self):
        agent = MaidAgent()
        agent._core = AsyncMock()
        agent._pid = 3
        config = MagicMock()
        config.name = "maid@local"

        await agent.on_init(config)
        self.assertFalse(agent._loop_task.done())

        await agent.on_shutdown("normal")
        # Loop should be cancelled.
        self.assertTrue(agent._loop_task.done())


if __name__ == "__main__":
    unittest.main()
