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


class TestCoreClientWrappers(unittest.IsolatedAsyncioTestCase):
    """Test CoreClient list_siblings and wait_child wrappers."""

    async def test_list_siblings(self):
        agent = HiveAgent()
        agent._core = AsyncMock()
        sibling = MagicMock(pid=5, name="sibling-a", role="worker", state="running")
        agent._core.list_siblings = AsyncMock(return_value=[
            {"pid": 5, "name": "sibling-a", "role": "worker", "state": "running"},
        ])

        result = await agent._core.list_siblings()
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]["pid"], 5)
        self.assertEqual(result[0]["name"], "sibling-a")

    async def test_wait_child(self):
        agent = HiveAgent()
        agent._core = AsyncMock()
        agent._core.wait_child = AsyncMock(return_value={
            "pid": 10, "exit_code": 0, "output": "done",
        })

        result = await agent._core.wait_child(10, timeout_seconds=30)
        self.assertEqual(result["pid"], 10)
        self.assertEqual(result["exit_code"], 0)
        self.assertEqual(result["output"], "done")


class TestSendAndWaitCore(unittest.IsolatedAsyncioTestCase):
    """Test send_and_wait_core method."""

    async def test_send_and_wait_core_resolves(self):
        from hivekernel_sdk.types import Message

        agent = HiveAgent()
        agent._core = AsyncMock()
        agent._core.send_message = AsyncMock(return_value="msg-123")

        async def simulate_reply():
            await asyncio.sleep(0.05)
            # Find the pending request and resolve it.
            for req_id, fut in list(agent._pending_requests.items()):
                reply = Message(
                    message_id="reply-1",
                    from_pid=10,
                    type="task_response",
                    payload=b'{"result": "ok"}',
                    reply_to=req_id,
                )
                if not fut.done():
                    fut.set_result(reply)

        asyncio.create_task(simulate_reply())

        result = await agent.send_and_wait_core(
            to_pid=10, type="task_request", payload=b"hello", timeout=5.0,
        )
        self.assertEqual(result.type, "task_response")
        self.assertEqual(result.from_pid, 10)

    async def test_send_and_wait_core_timeout(self):
        agent = HiveAgent()
        agent._core = AsyncMock()
        agent._core.send_message = AsyncMock(return_value="msg-456")

        with self.assertRaises(TimeoutError):
            await agent.send_and_wait_core(
                to_pid=10, type="task_request", payload=b"hello", timeout=0.1,
            )
        # Pending request should be cleaned up.
        self.assertEqual(len(agent._pending_requests), 0)

    async def test_send_and_wait_core_no_client(self):
        agent = HiveAgent()
        agent._core = None

        with self.assertRaises(RuntimeError):
            await agent.send_and_wait_core(
                to_pid=10, type="task_request", payload=b"hello",
            )


if __name__ == "__main__":
    unittest.main()
