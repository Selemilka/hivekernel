"""QueenAgent -- task dispatcher daemon for HiveKernel.

Receives tasks, assesses complexity (history -> heuristics -> LLM),
routes to execution strategy:
- Simple task -> spawn task-role worker, single LLM call, collect result
- Complex task -> spawn/reuse lead (orchestrator), full decomposition pipeline
- Architect task -> spawn architect for strategic plan, then lead executes

Queen is a pure application agent (Layer 5). She does NOT spawn system daemons --
daemon spawning is handled by the kernel's startup config (configs/startup-full.json).

Features:
- Lead reuse: keeps orchestrator leads alive between tasks (saves ~5s spawn)
- Heuristic complexity: keyword/length check before LLM (saves tokens)
- Task history: remembers recent results to inform routing
- Maid integration: reads health report before complex tasks
- Architect routing: strategic planner for design/architecture tasks
- Mailbox-first: unified code path via CoreClient for both IPC and Execute stream

Runtime image: hivekernel_sdk.queen:QueenAgent
"""

import asyncio
import json
import logging
import time
import uuid
from collections import deque

from .llm_agent import LLMAgent
from .syscall import SyscallContext
from .types import Message, MessageAck, TaskResult

logger = logging.getLogger("hivekernel.queen")

# Lead reuse settings.
LEAD_IDLE_TIMEOUT = 300  # 5 minutes
LEAD_REAPER_INTERVAL = 60  # seconds between reaper checks

# Complexity heuristic keywords.
_SIMPLE_KEYWORDS = frozenset({
    "translate", "summarize", "define", "explain", "convert",
    "format", "count", "list", "describe", "what",
})
_COMPLEX_KEYWORDS = frozenset({
    "research", "analyze", "compare", "investigate", "plan",
    "design", "implement", "evaluate", "review", "create",
    "develop", "build", "optimize", "benchmark",
})

# Architect keywords -- trigger strategic planning path.
_ARCHITECT_KEYWORDS = frozenset({
    "architect", "architecture", "blueprint", "roadmap",
    "infrastructure", "framework",
})

MAX_HISTORY = 20
SIMILARITY_THRESHOLD = 0.5


def _word_similarity(a: str, b: str) -> float:
    """Jaccard similarity between word sets of two strings."""
    wa = set(a.lower().split())
    wb = set(b.lower().split())
    if not wa or not wb:
        return 0.0
    return len(wa & wb) / len(wa | wb)


class QueenAgent(LLMAgent):
    """Local coordinator daemon. Receives tasks, decides execution strategy."""

    def __init__(self):
        super().__init__()
        self._idle_leads: list[tuple[int, float]] = []  # (pid, idle_since)
        self._task_history: deque = deque(maxlen=MAX_HISTORY)
        self._reaper_task = None
        self._max_workers: str = "3"

    async def on_init(self, config):
        """Start lead reaper on startup."""
        await super().on_init(config)
        self._reaper_task = asyncio.create_task(self._lead_reaper())

    async def on_shutdown(self, reason):
        """Kill idle leads and cancel reaper on shutdown."""
        if self._reaper_task and not self._reaper_task.done():
            self._reaper_task.cancel()
            try:
                await self._reaper_task
            except asyncio.CancelledError:
                pass
        # Kill all idle leads.
        for pid, _ in self._idle_leads:
            try:
                await self.core.kill_child(pid)
            except Exception:
                pass
        self._idle_leads.clear()
        return None

    async def _lead_reaper(self):
        """Background: kill leads that have been idle too long."""
        while True:
            await asyncio.sleep(LEAD_REAPER_INTERVAL)
            now = time.time()
            still_idle = []
            for pid, idle_since in self._idle_leads:
                if now - idle_since > LEAD_IDLE_TIMEOUT:
                    try:
                        await self.core.kill_child(pid)
                        logger.info("Reaped idle lead PID %d (idle %.0fs)",
                                    pid, now - idle_since)
                    except Exception:
                        pass  # Already dead
                else:
                    still_idle.append((pid, idle_since))
            self._idle_leads = still_idle

    # --- Message Handling (async IPC from siblings) ---

    async def handle_message(self, message: Message) -> MessageAck:
        """Handle incoming IPC messages from sibling agents."""
        if message.type in ("task_request", "cron_task"):
            # Queue the task for async processing.
            asyncio.create_task(self._process_message_task(message))
            return MessageAck(status=MessageAck.ACK_QUEUED)
        return MessageAck(status=MessageAck.ACK_ACCEPTED)

    async def _process_message_task(self, message: Message):
        """Process a task_request received via IPC and send result back.

        Uses the unified _core_handle_* methods (same code path as handle_task).
        """
        try:
            payload = json.loads(message.payload.decode("utf-8"))
            description = payload.get("task", payload.get("description", ""))
            trace_id = payload.get("trace_id", "") or str(uuid.uuid4())
            if self.llm and self.llm._dialog_logger:
                self.llm._dialog_logger.trace_id = trace_id
            if not description:
                result_payload = json.dumps({"error": "empty task"}).encode("utf-8")
            else:
                logger.info("Queen processing IPC task from PID %d: %s",
                            message.from_pid, description[:80])

                # Check Maid health report.
                health_warning = await self._check_maid_health()
                if health_warning:
                    await self.core.log("warn", f"Maid health: {health_warning}")

                # Assess complexity.
                complexity = await self._assess_complexity(description)
                logger.info("IPC task complexity: %s", complexity)

                # Execute using unified core methods.
                if complexity == "simple":
                    result = await self._core_handle_simple(description, trace_id=trace_id)
                elif complexity == "architect":
                    result = await self._core_handle_architect(description, trace_id=trace_id)
                else:
                    result = await self._core_handle_complex(description, trace_id=trace_id)

                # Record in task history.
                self._task_history.append({
                    "task": description,
                    "complexity": complexity,
                    "exit_code": result["exit_code"],
                    "ts": time.time(),
                })

                # Store result artifact.
                safe_key = description.lower().replace(" ", "-")[:40]
                try:
                    await self.core.store_artifact(
                        key=f"queen-result-{safe_key}",
                        content=json.dumps({
                            "task": description,
                            "complexity": complexity,
                            "output": result["output"][:4000],
                            "exit_code": result["exit_code"],
                        }, ensure_ascii=False).encode("utf-8"),
                        content_type="application/json",
                    )
                except Exception as e:
                    logger.warning("Failed to store artifact: %s", e)

                result_payload = json.dumps({
                    "output": result["output"],
                    "exit_code": result["exit_code"],
                    "metadata": result.get("metadata", {}),
                }, ensure_ascii=False).encode("utf-8")
        except Exception as e:
            logger.error("Queen IPC task failed: %s", e)
            result_payload = json.dumps({
                "error": str(e),
                "exit_code": 1,
            }).encode("utf-8")

        # Send reply back to the sender.
        if self.core and message.from_pid:
            try:
                await self.core.send_message(
                    to_pid=message.from_pid,
                    type="task_response",
                    payload=result_payload,
                    reply_to=message.reply_to,
                )
            except Exception as e:
                logger.error("Queen failed to send reply to PID %d: %s",
                             message.from_pid, e)

    # --- Task Handling (Execute stream -- thin wrapper) ---

    async def handle_task(self, task, ctx: SyscallContext) -> TaskResult:
        """Execute stream entry point. Delegates to unified _core_handle_* methods."""
        description = task.params.get("task", task.description)
        self._max_workers = task.params.get("max_workers", "3")
        trace_id = task.params.get("trace_id", "") or str(uuid.uuid4())
        if self.llm and self.llm._dialog_logger:
            self.llm._dialog_logger.trace_id = trace_id
        if not description.strip():
            return TaskResult(exit_code=1, output="Empty task description")

        await ctx.report_progress("Assessing task complexity...", 5.0)
        await self.core.log("info", f"Queen received task: {description[:100]}")

        # 1. Check Maid health report before execution.
        health_warning = await self._check_maid_health()
        if health_warning:
            await self.core.log("warn", f"Maid health: {health_warning}")

        # 2. Assess complexity (history -> heuristics -> LLM).
        complexity = await self._assess_complexity(description)
        await self.core.log("info", f"Task complexity: {complexity}")

        # 3. Execute based on complexity (unified core methods).
        if complexity == "simple":
            await ctx.report_progress("Spawning task worker...", 15.0)
            result = await self._core_handle_simple(description, trace_id=trace_id)
        elif complexity == "architect":
            await ctx.report_progress("Spawning architect...", 15.0)
            result = await self._core_handle_architect(description, trace_id=trace_id)
        else:
            await ctx.report_progress("Acquiring orchestrator...", 15.0)
            result = await self._core_handle_complex(description, trace_id=trace_id)

        # 4. Record in task history.
        self._task_history.append({
            "task": description,
            "complexity": complexity,
            "exit_code": result["exit_code"],
            "ts": time.time(),
        })

        # 5. Store result artifact.
        await ctx.report_progress("Storing result...", 95.0)
        safe_key = description.lower().replace(" ", "-")[:40]
        try:
            await self.core.store_artifact(
                key=f"queen-result-{safe_key}",
                content=json.dumps({
                    "task": description,
                    "complexity": complexity,
                    "output": result["output"][:4000],
                    "exit_code": result["exit_code"],
                }, ensure_ascii=False).encode("utf-8"),
                content_type="application/json",
            )
        except Exception as e:
            logger.warning("Failed to store artifact: %s", e)

        await self.core.log("info", f"Queen task done (exit={result['exit_code']})")
        return TaskResult(
            exit_code=result["exit_code"],
            output=result["output"],
            artifacts=result.get("artifacts", {}),
            metadata=result.get("metadata", {}),
        )

    # --- Complexity Assessment ---

    async def _assess_complexity(self, description: str) -> str:
        """Three-tier assessment: history -> heuristics -> LLM."""
        # 1. Check task history for similar past tasks.
        for entry in self._task_history:
            if _word_similarity(entry["task"], description) >= SIMILARITY_THRESHOLD:
                logger.info("Complexity from history: %s (similar to '%s')",
                            entry["complexity"], entry["task"][:50])
                return entry["complexity"]

        # 2. Heuristic check: keywords + length.
        lower = description.lower()
        words = set(lower.split())
        architect_hits = len(words & _ARCHITECT_KEYWORDS)
        simple_hits = len(words & _SIMPLE_KEYWORDS)
        complex_hits = len(words & _COMPLEX_KEYWORDS)

        if architect_hits >= 1:
            logger.info("Complexity heuristic: architect (architect keywords)")
            return "architect"
        if len(description) < 80 and simple_hits > 0 and complex_hits == 0:
            logger.info("Complexity heuristic: simple (short + simple keywords)")
            return "simple"
        if len(description) > 200 or complex_hits >= 2:
            logger.info("Complexity heuristic: complex (long or complex keywords)")
            return "complex"

        # 3. Ambiguous: fall back to LLM.
        return await self._assess_complexity_llm(description)

    async def _assess_complexity_llm(self, description: str) -> str:
        """LLM-based complexity assessment (fallback for ambiguous cases)."""
        try:
            response = await self.ask(
                "You are a task complexity assessor. Determine the execution "
                "strategy for the following task.\n\n"
                f"Task: {description}\n\n"
                "Options:\n"
                '- "simple": single step, one agent can handle it\n'
                '- "complex": needs decomposition into multiple subtasks\n'
                '- "architect": needs strategic planning/design before execution\n\n'
                "Reply with ONLY a JSON object: "
                "{\"complexity\": \"simple\"} or "
                "{\"complexity\": \"complex\"} or "
                "{\"complexity\": \"architect\"}. No other text.",
                max_tokens=64,
                temperature=0.0,
            )
            text = response.strip()
            if text.startswith("```"):
                lines = text.split("\n")
                text = "\n".join(l for l in lines if not l.strip().startswith("```"))
            data = json.loads(text)
            complexity = data.get("complexity", "complex")
            if complexity in ("simple", "complex", "architect"):
                return complexity
        except Exception as e:
            logger.warning("LLM complexity assessment failed: %s, defaulting to complex", e)
        return "complex"

    # --- Maid Integration ---

    async def _check_maid_health(self) -> str:
        """Read latest Maid health report artifact. Returns warning or empty."""
        if not self.core:
            return ""
        try:
            artifact = await self.core.get_artifact(key="maid/health-report")
            report = json.loads(artifact.content.decode("utf-8"))
            if report.get("anomalies"):
                return "; ".join(report["anomalies"])
        except Exception:
            pass  # No report yet or Maid not spawned
        return ""

    # --- Lead Management ---

    async def _acquire_lead(self, ctx: SyscallContext = None) -> int:
        """Get a lead PID: reuse idle lead or spawn new one.

        When ctx is provided, spawns via SyscallContext (Execute stream).
        When ctx is None, spawns via self.core (CoreClient).
        """
        while self._idle_leads:
            pid, _ = self._idle_leads.pop(0)
            try:
                info = await self.core.get_process_info(pid)
                if info.state in (0, 1):  # STATE_IDLE or STATE_RUNNING
                    logger.info("Reusing idle lead PID %d", pid)
                    return pid
                logger.info("Idle lead PID %d not alive (state=%d), skip",
                            pid, info.state)
            except Exception:
                logger.info("Idle lead PID %d gone, skip", pid)

        # No reusable lead: spawn new one.
        if ctx is not None:
            lead_pid = await ctx.spawn(
                name="queen-lead",
                role="lead",
                cognitive_tier="tactical",
                model="sonnet",
                system_prompt=(
                    "You are a task orchestrator. You decompose tasks, "
                    "delegate to workers, and synthesize results."
                ),
                runtime_image="hivekernel_sdk.orchestrator:OrchestratorAgent",
                runtime_type="python",
            )
        else:
            lead_pid = await self.core.spawn_child(
                name="queen-lead",
                role="lead",
                cognitive_tier="tactical",
                model="sonnet",
                system_prompt=(
                    "You are a task orchestrator. You decompose tasks, "
                    "delegate to workers, and synthesize results."
                ),
                runtime_image="hivekernel_sdk.orchestrator:OrchestratorAgent",
                runtime_type="python",
            )
        logger.info("Spawned new lead PID %d", lead_pid)
        return lead_pid

    async def _release_lead(self, lead_pid: int):
        """Return a lead to the idle pool instead of killing it."""
        self._idle_leads.append((lead_pid, time.time()))
        logger.info("Lead PID %d returned to idle pool (%d idle)",
                     lead_pid, len(self._idle_leads))

    # --- Core-based Task Execution (unified path) ---

    async def _core_handle_simple(self, description: str, trace_id: str = "") -> dict:
        """Simple task: spawn worker via CoreClient, execute, collect."""
        worker_pid = None
        try:
            await self.core.log("info", "Simple strategy: spawning task worker")
            worker_pid = await self.core.spawn_child(
                name="queen-task",
                role="task",
                cognitive_tier="operational",
                model="mini",
                system_prompt=(
                    "You are a skilled assistant. Complete the given task "
                    "thoroughly and concisely."
                ),
                runtime_image="hivekernel_sdk.worker:WorkerAgent",
                runtime_type="python",
            )
            logger.info("Spawned worker PID %d for simple task", worker_pid)
            params = {"subtask": description}
            if trace_id:
                params["trace_id"] = trace_id
            result = await self.core.execute_task(
                target_pid=worker_pid,
                description=description,
                params=params,
                timeout_seconds=120,
            )
            # Task-role workers auto-exit; kill to be safe.
            try:
                await self.core.kill_child(worker_pid)
            except Exception:
                pass
            return {**result, "metadata": {**result.get("metadata", {}), "strategy": "simple"}}
        except Exception as e:
            logger.error("Simple task failed: %s", e)
            if worker_pid:
                try:
                    await self.core.kill_child(worker_pid)
                except Exception:
                    pass
            return {"output": f"Task failed: {e}", "exit_code": 1}

    async def _core_handle_complex(self, description: str, trace_id: str = "") -> dict:
        """Complex task: acquire lead (reuse or spawn), delegate, collect."""
        await self.core.log("info", "Complex strategy: acquiring orchestrator lead")
        lead_pid = await self._acquire_lead()

        try:
            params = {"task": description, "max_workers": self._max_workers}
            if trace_id:
                params["trace_id"] = trace_id
            result = await self.core.execute_task(
                target_pid=lead_pid,
                description=description,
                params=params,
                timeout_seconds=300,
            )
            # Success: return lead to idle pool for reuse.
            await self._release_lead(lead_pid)
            return {**result, "metadata": {**result.get("metadata", {}),
                                           "strategy": "complex",
                                           "lead_pid": str(lead_pid)}}
        except Exception as e:
            logger.error("Orchestrator lead failed: %s", e)
            try:
                await self.core.kill_child(lead_pid)
            except Exception:
                pass
            return {"output": f"Task failed: {e}", "exit_code": 1}

    async def _core_handle_architect(self, description: str, trace_id: str = "") -> dict:
        """Architect task: strategic plan -> lead execution."""
        await self.core.log("info", "Architect strategy: spawning strategic planner")

        # 1. Spawn Architect (task role, auto-exits after plan).
        arch_pid = await self.core.spawn_child(
            name="architect",
            role="task",
            cognitive_tier="tactical",
            model="sonnet",
            system_prompt=(
                "You are a strategic architect. Analyze tasks thoroughly, "
                "identify challenges, and produce detailed execution plans."
            ),
            runtime_image="hivekernel_sdk.architect:ArchitectAgent",
            runtime_type="python",
        )
        await self.core.log("info", f"Architect spawned as PID {arch_pid}")

        # 2. Get plan from Architect.
        try:
            arch_params = {"task": description, "max_workers": self._max_workers}
            if trace_id:
                arch_params["trace_id"] = trace_id
            plan_result = await self.core.execute_task(
                target_pid=arch_pid,
                description=description,
                params=arch_params,
                timeout_seconds=120,
            )
        except Exception as e:
            await self.core.log("error", f"Architect failed: {e}, falling back to complex")
            return await self._core_handle_complex(description, trace_id=trace_id)

        plan_json = plan_result["output"]
        await self.core.log("info", f"Architect plan received ({len(plan_json)} bytes)")

        # 3. Acquire lead and execute with the Architect's plan.
        lead_pid = await self._acquire_lead()

        try:
            lead_params = {
                "task": description,
                "plan": plan_json,
                "max_workers": self._max_workers,
            }
            if trace_id:
                lead_params["trace_id"] = trace_id
            result = await self.core.execute_task(
                target_pid=lead_pid,
                description=description,
                params=lead_params,
                timeout_seconds=300,
            )
            await self._release_lead(lead_pid)
            return {**result, "metadata": {**result.get("metadata", {}),
                                           "strategy": "architect",
                                           "architect_pid": str(arch_pid)}}
        except Exception as e:
            await self.core.log("error", f"Lead execution failed: {e}")
            try:
                await self.core.kill_child(lead_pid)
            except Exception:
                pass
            return {"output": f"Architect execution failed: {e}", "exit_code": 1}
