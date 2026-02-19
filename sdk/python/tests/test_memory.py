"""Unit tests for memory.py -- AgentMemory.

Run: python -m pytest sdk/python/tests/test_memory.py -v
"""

import json
import os
import sys
import unittest
from unittest.mock import AsyncMock, MagicMock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from hivekernel_sdk.memory import AgentMemory


class TestAddMessage(unittest.TestCase):
    def test_simple_message(self):
        mem = AgentMemory(pid=42)
        mem.add_message("user", "hello")
        self.assertEqual(len(mem.messages), 1)
        self.assertEqual(mem.messages[0], {"role": "user", "content": "hello"})

    def test_assistant_with_tool_calls(self):
        mem = AgentMemory(pid=42)
        tc = [{"id": "tc1", "function": {"name": "foo", "arguments": "{}"}}]
        mem.add_message("assistant", "", tool_calls=tc)
        self.assertEqual(mem.messages[0]["tool_calls"], tc)

    def test_tool_result_message(self):
        mem = AgentMemory(pid=42)
        mem.add_message("tool", "result text", tool_call_id="tc1")
        self.assertEqual(mem.messages[0]["tool_call_id"], "tc1")

    def test_no_extra_keys(self):
        mem = AgentMemory(pid=42)
        mem.add_message("user", "hello")
        self.assertNotIn("tool_calls", mem.messages[0])
        self.assertNotIn("tool_call_id", mem.messages[0])


class TestGetContextMessages(unittest.TestCase):
    def test_system_prompt_only(self):
        mem = AgentMemory(pid=42)
        msgs = mem.get_context_messages("You are helpful.")
        self.assertEqual(len(msgs), 1)
        self.assertEqual(msgs[0]["role"], "system")
        self.assertIn("You are helpful.", msgs[0]["content"])

    def test_injects_long_term_memory(self):
        mem = AgentMemory(pid=42)
        mem.long_term = "I prefer Python."
        msgs = mem.get_context_messages("You are helpful.")
        self.assertIn("I prefer Python.", msgs[0]["content"])
        self.assertIn("Long-term Memory", msgs[0]["content"])

    def test_injects_summary(self):
        mem = AgentMemory(pid=42)
        mem.summary = "We discussed file operations."
        msgs = mem.get_context_messages("You are helpful.")
        self.assertIn("We discussed file operations.", msgs[0]["content"])
        self.assertIn("Conversation Summary", msgs[0]["content"])

    def test_includes_session_messages(self):
        mem = AgentMemory(pid=42)
        mem.add_message("user", "hello")
        mem.add_message("assistant", "hi there")
        msgs = mem.get_context_messages("System prompt")
        self.assertEqual(len(msgs), 3)
        self.assertEqual(msgs[1]["role"], "user")
        self.assertEqual(msgs[2]["role"], "assistant")


class TestNeedsSummarization(unittest.TestCase):
    def test_below_threshold(self):
        mem = AgentMemory(pid=42)
        for i in range(10):
            mem.add_message("user", f"msg {i}")
        self.assertFalse(mem.needs_summarization(threshold=20))

    def test_at_threshold(self):
        mem = AgentMemory(pid=42)
        for i in range(20):
            mem.add_message("user", f"msg {i}")
        self.assertFalse(mem.needs_summarization(threshold=20))

    def test_above_threshold(self):
        mem = AgentMemory(pid=42)
        for i in range(21):
            mem.add_message("user", f"msg {i}")
        self.assertTrue(mem.needs_summarization(threshold=20))

    def test_custom_threshold(self):
        mem = AgentMemory(pid=42)
        for i in range(6):
            mem.add_message("user", f"msg {i}")
        self.assertTrue(mem.needs_summarization(threshold=5))


class TestEstimateTokens(unittest.TestCase):
    def test_empty(self):
        mem = AgentMemory(pid=42)
        self.assertEqual(mem.estimate_tokens(), 0)

    def test_rough_estimate(self):
        mem = AgentMemory(pid=42)
        mem.add_message("user", "a" * 250)  # ~100 tokens
        tokens = mem.estimate_tokens()
        self.assertAlmostEqual(tokens, 100, delta=5)


class TestForceCompress(unittest.TestCase):
    def test_compress_drops_oldest(self):
        mem = AgentMemory(pid=42)
        for i in range(10):
            mem.add_message("user", f"msg {i}")
        mem.force_compress()
        # Should keep last 5 (50% of 10)
        self.assertEqual(len(mem.messages), 5)
        self.assertEqual(mem.messages[0]["content"], "msg 5")

    def test_compress_keeps_minimum(self):
        mem = AgentMemory(pid=42)
        mem.add_message("user", "msg 0")
        mem.add_message("assistant", "msg 1")
        mem.force_compress()
        # 2 messages, should not drop anything
        self.assertEqual(len(mem.messages), 2)

    def test_compress_empty(self):
        mem = AgentMemory(pid=42)
        mem.force_compress()
        self.assertEqual(len(mem.messages), 0)


class TestTokenBasedSummarization(unittest.TestCase):
    def test_token_trigger(self):
        mem = AgentMemory(pid=42)
        # Add a large message that exceeds 60000 tokens (~150KB of text)
        mem.add_message("user", "x" * 200000)
        self.assertTrue(mem.needs_summarization(threshold=100, max_tokens=60000))

    def test_count_trigger_still_works(self):
        mem = AgentMemory(pid=42)
        for i in range(25):
            mem.add_message("user", "short")
        self.assertTrue(mem.needs_summarization(threshold=20, max_tokens=1000000))


class TestLoadSave(unittest.IsolatedAsyncioTestCase):
    async def test_save_stores_all_keys(self):
        mem = AgentMemory(pid=7)
        mem.long_term = "knowledge"
        mem.summary = "recap"
        mem.add_message("user", "hello")

        ctx = AsyncMock()
        await mem.save(ctx)

        self.assertEqual(ctx.store_artifact.call_count, 3)
        calls = {c.args[0]: c.args[1] for c in ctx.store_artifact.call_args_list}
        self.assertEqual(calls["agent:7:memory"], b"knowledge")
        self.assertEqual(calls["agent:7:summary"], b"recap")
        session_data = json.loads(calls["agent:7:session"])
        self.assertEqual(len(session_data), 1)

    async def test_load_restores_all_keys(self):
        ctx = AsyncMock()

        def fake_get(key):
            data = {
                "agent:7:memory": b"knowledge",
                "agent:7:session": json.dumps([{"role": "user", "content": "hi"}]).encode(),
                "agent:7:summary": b"recap",
            }
            result = MagicMock()
            result.content = data.get(key, b"")
            return result

        ctx.get_artifact = AsyncMock(side_effect=fake_get)

        mem = AgentMemory(pid=7)
        await mem.load(ctx)

        self.assertEqual(mem.long_term, "knowledge")
        self.assertEqual(mem.summary, "recap")
        self.assertEqual(len(mem.messages), 1)
        self.assertEqual(mem.messages[0]["content"], "hi")

    async def test_load_tolerates_missing(self):
        ctx = AsyncMock()
        ctx.get_artifact = AsyncMock(side_effect=Exception("not found"))

        mem = AgentMemory(pid=99)
        await mem.load(ctx)

        self.assertEqual(mem.long_term, "")
        self.assertEqual(mem.messages, [])
        self.assertEqual(mem.summary, "")


if __name__ == "__main__":
    unittest.main()
