"""LLMAgent -- HiveAgent subclass with built-in OpenRouter LLM client."""

import os

from .agent import HiveAgent
from .llm import LLMClient, MODEL_MAP
from .types import AgentConfig


class LLMAgent(HiveAgent):
    """HiveAgent that auto-creates an LLMClient from AgentConfig.

    Subclass this instead of HiveAgent when your agent needs LLM access.
    Implement handle_task() and use self.ask() / self.chat() for LLM calls.

    Requires OPENROUTER_API_KEY environment variable.
    """

    def __init__(self):
        super().__init__()
        self.llm: LLMClient | None = None
        self._system_prompt: str = ""

    async def on_init(self, config: AgentConfig) -> None:
        api_key = os.environ.get("OPENROUTER_API_KEY", "")
        if not api_key:
            raise RuntimeError("OPENROUTER_API_KEY not set")
        model_id = MODEL_MAP.get(config.model, config.model) if config.model else ""
        self.llm = LLMClient(api_key, default_model=model_id)
        self._system_prompt = config.system_prompt

    async def ask(self, prompt: str, **kwargs) -> str:
        """Shortcut: prompt -> response using agent's system_prompt and model.

        Args:
            prompt: The user message.
            **kwargs: Passed to LLMClient.complete() (model, max_tokens, temperature).

        Returns:
            The assistant's response text.
        """
        return await self.llm.complete(prompt, system=self._system_prompt, **kwargs)

    async def chat(self, messages: list[dict], **kwargs) -> str:
        """Multi-turn chat using agent's system_prompt and model.

        Args:
            messages: List of {"role": ..., "content": ...} dicts.
            **kwargs: Passed to LLMClient.chat() (model, max_tokens, temperature).

        Returns:
            The assistant's response text.
        """
        return await self.llm.chat(messages, system=self._system_prompt, **kwargs)
