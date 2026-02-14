"""Unit tests for HiveAgent (agent.py).

Run: python -m pytest sdk/python/tests/test_agent.py -v
  or: python sdk/python/tests/test_agent.py
"""

import asyncio
import os
import sys
import unittest
from unittest.mock import AsyncMock, MagicMock

# Allow running from project root.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from hivekernel_sdk.agent import HiveAgent


class TestShutdownStopsServer(unittest.IsolatedAsyncioTestCase):
    """Shutdown RPC must trigger server.stop() so the process actually exits."""

    async def test_shutdown_calls_server_stop(self):
        agent = HiveAgent()
        mock_server = AsyncMock()
        agent._server = mock_server

        servicer = agent._make_servicer()

        request = MagicMock()
        request.reason = 0  # SHUTDOWN_NORMAL
        context = MagicMock()

        resp = await servicer.Shutdown(request, context)

        # Let the asyncio.create_task(server.stop(...)) run.
        await asyncio.sleep(0)

        mock_server.stop.assert_called_once_with(grace=2)
        self.assertEqual(resp.state_snapshot, b"")

    async def test_shutdown_without_server_no_crash(self):
        """If _server is None, Shutdown must still work (no crash)."""
        agent = HiveAgent()
        agent._server = None

        servicer = agent._make_servicer()

        request = MagicMock()
        request.reason = 0
        context = MagicMock()

        resp = await servicer.Shutdown(request, context)
        self.assertEqual(resp.state_snapshot, b"")

    async def test_shutdown_calls_on_shutdown(self):
        """Shutdown must call agent.on_shutdown() and return its snapshot."""

        class SnapshotAgent(HiveAgent):
            async def on_shutdown(self, reason):
                return b"my-state-data"

        agent = SnapshotAgent()
        agent._server = AsyncMock()

        servicer = agent._make_servicer()

        request = MagicMock()
        request.reason = 4  # USER_REQUEST
        context = MagicMock()

        resp = await servicer.Shutdown(request, context)
        self.assertEqual(resp.state_snapshot, b"my-state-data")


if __name__ == "__main__":
    unittest.main()
