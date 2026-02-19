"""Example ToolAgent with a calculator tool.

Demonstrates the AgentCore: tool calling, agent loop, memory.
Used by test_tool_agent_e2e.py for E2E testing.

Usage (standalone, requires running kernel):
    python -m hivekernel_sdk.runner --agent examples.calculator_agent:CalculatorAgent --core localhost:50051
"""

from hivekernel_sdk.tools import Tool, ToolContext, ToolResult
from hivekernel_sdk.tool_agent import ToolAgent


class CalculatorTool:
    """Simple calculator tool the LLM can call."""

    @property
    def name(self) -> str:
        return "calculator"

    @property
    def description(self) -> str:
        return "Evaluate a math expression. Returns the numeric result."

    @property
    def parameters(self) -> dict:
        return {
            "type": "object",
            "properties": {
                "expression": {
                    "type": "string",
                    "description": "Math expression to evaluate, e.g. '2 + 3 * 4'",
                },
            },
            "required": ["expression"],
        }

    async def execute(self, ctx: ToolContext, args: dict) -> ToolResult:
        expr = args.get("expression", "")
        try:
            # Safe eval: only allow math operations
            allowed = {"__builtins__": {}}
            result = eval(expr, allowed)
            await ctx.log("info", f"Calculator: {expr} = {result}")
            return ToolResult(content=str(result))
        except Exception as e:
            return ToolResult(content=f"Error: {e}", is_error=True)


class CalculatorAgent(ToolAgent):
    """Agent that can do math via tool calling."""

    def get_tools(self) -> list:
        return [CalculatorTool()]
