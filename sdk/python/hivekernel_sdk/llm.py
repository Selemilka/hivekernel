"""Async OpenRouter LLM client. Zero extra deps -- uses urllib + asyncio.to_thread."""

import asyncio
import json
import urllib.request

URL = "https://openrouter.ai/api/v1/chat/completions"

MODEL_MAP = {
    "opus": "anthropic/claude-opus-4",
    "sonnet": "anthropic/claude-sonnet-4-5",
    "mini": "google/gemini-2.5-flash",
    "haiku": "anthropic/claude-haiku-3-5",
}


class LLMClient:
    """Async OpenRouter chat completion client.

    Uses urllib.request (stdlib) wrapped in asyncio.to_thread to avoid
    extra dependencies. Tracks cumulative token usage.
    """

    def __init__(self, api_key: str, default_model: str = ""):
        self._api_key = api_key
        self._default_model = default_model or MODEL_MAP["mini"]
        self._total_tokens = 0

    @property
    def total_tokens(self) -> int:
        """Cumulative token usage across all calls."""
        return self._total_tokens

    async def chat(
        self,
        messages: list[dict],
        model: str = "",
        system: str = "",
        max_tokens: int = 4096,
        temperature: float = 0.7,
    ) -> str:
        """Send a chat completion request and return the response text.

        Args:
            messages: List of {"role": ..., "content": ...} dicts.
            model: OpenRouter model ID (or MODEL_MAP key). Falls back to default.
            system: Optional system prompt (prepended as system message).
            max_tokens: Max tokens in the response.
            temperature: Sampling temperature.

        Returns:
            The assistant's response text.

        Raises:
            RuntimeError: On HTTP or API errors.
        """
        resolved = MODEL_MAP.get(model, model) if model else self._default_model

        full_messages = list(messages)
        if system:
            full_messages.insert(0, {"role": "system", "content": system})

        body = {
            "model": resolved,
            "messages": full_messages,
            "max_tokens": max_tokens,
            "temperature": temperature,
        }

        data = json.dumps(body).encode("utf-8")
        req = urllib.request.Request(
            URL,
            data=data,
            headers={
                "Authorization": f"Bearer {self._api_key}",
                "Content-Type": "application/json",
            },
            method="POST",
        )

        resp_data = await asyncio.to_thread(self._do_request, req)
        resp = json.loads(resp_data)

        if "error" in resp:
            err = resp["error"]
            msg = err.get("message", str(err)) if isinstance(err, dict) else str(err)
            raise RuntimeError(f"OpenRouter API error: {msg}")

        usage = resp.get("usage", {})
        self._total_tokens += usage.get("total_tokens", 0)

        choices = resp.get("choices", [])
        if not choices:
            raise RuntimeError("OpenRouter returned no choices")

        return choices[0]["message"]["content"]

    async def chat_with_tools(
        self,
        messages: list[dict],
        tools: list[dict] | None = None,
        model: str = "",
        system: str = "",
        max_tokens: int = 4096,
        temperature: float = 0.7,
    ) -> dict:
        """Chat with tool support. Returns full choice dict.

        Returns:
            {"message": {"role", "content", "tool_calls"}, "finish_reason": "stop"|"tool_calls"}
        """
        resolved = MODEL_MAP.get(model, model) if model else self._default_model

        full_messages = list(messages)
        if system:
            full_messages.insert(0, {"role": "system", "content": system})

        body = {
            "model": resolved,
            "messages": full_messages,
            "max_tokens": max_tokens,
            "temperature": temperature,
        }
        if tools:
            body["tools"] = tools
            body["tool_choice"] = "auto"

        data = json.dumps(body).encode("utf-8")
        req = urllib.request.Request(
            URL,
            data=data,
            headers={
                "Authorization": f"Bearer {self._api_key}",
                "Content-Type": "application/json",
            },
            method="POST",
        )

        resp_data = await asyncio.to_thread(self._do_request, req)
        resp = json.loads(resp_data)

        if "error" in resp:
            err = resp["error"]
            msg = err.get("message", str(err)) if isinstance(err, dict) else str(err)
            raise RuntimeError(f"OpenRouter API error: {msg}")

        usage = resp.get("usage", {})
        self._total_tokens += usage.get("total_tokens", 0)

        choices = resp.get("choices", [])
        if not choices:
            raise RuntimeError("OpenRouter returned no choices")

        choice = choices[0]
        message = choice.get("message", {})
        # Normalize: content can be null when model calls tools
        if message.get("content") is None:
            message["content"] = ""

        return {
            "message": message,
            "finish_reason": choice.get("finish_reason", "stop"),
        }

    async def complete(self, prompt: str, **kwargs) -> str:
        """Shortcut: single user prompt -> response text.

        Args:
            prompt: The user message.
            **kwargs: Passed to chat() (model, system, max_tokens, temperature).

        Returns:
            The assistant's response text.
        """
        messages = [{"role": "user", "content": prompt}]
        return await self.chat(messages, **kwargs)

    @staticmethod
    def _do_request(req: urllib.request.Request) -> str:
        """Synchronous HTTP call (runs in thread via asyncio.to_thread)."""
        try:
            with urllib.request.urlopen(req, timeout=120) as resp:
                return resp.read().decode("utf-8")
        except urllib.error.HTTPError as e:
            body = e.read().decode("utf-8", errors="replace")
            raise RuntimeError(
                f"OpenRouter HTTP {e.code}: {body[:500]}"
            ) from e
