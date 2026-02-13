"""
HiveKernel Demo: LLM Research Team
====================================

Same structure as demo_showcase.py, but with REAL LLM calls via OpenRouter.

  1. Starts the Go kernel (bin/hivekernel.exe)
  2. Spawns LLMResearchLead (tactical/sonnet) -- real Python process
  3. Lead asks LLM to break topic into subtopics
  4. Lead spawns LLMResearchWorker per subtopic (operational/mini)
  5. Each worker asks LLM to research its subtopic
  6. Lead asks LLM to synthesize findings into a report
  7. Report stored as artifact, workers killed, results printed

Requires:
    - .env file with OPENROUTER_API_KEY=sk-or-v1-...
    - Built kernel: go build -o bin/hivekernel.exe ./cmd/hivekernel
    - pip install fastapi uvicorn (for dashboard)

Usage:
    python sdk/python/examples/llm_research_team.py               # with dashboard
    python sdk/python/examples/llm_research_team.py --no-dashboard # without dashboard
"""

import asyncio
import json
import os
import subprocess
import sys
import time
import webbrowser

SDK_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, SDK_DIR)

import grpc.aio  # noqa: E402
from hivekernel_sdk import agent_pb2, agent_pb2_grpc, core_pb2, core_pb2_grpc  # noqa: E402
from hivekernel_sdk import AgentConfig, TaskResult  # noqa: E402
from hivekernel_sdk.llm_agent import LLMAgent  # noqa: E402
from hivekernel_sdk.syscall import SyscallContext  # noqa: E402

KERNEL_BIN = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "..", "..", "bin", "hivekernel.exe")
)
DASHBOARD_APP = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "dashboard", "app.py")
)
CORE_ADDR = "localhost:50051"
DASHBOARD_PORT = 8080
QUEEN_PID = 2
QUEEN_MD = [("x-hivekernel-pid", str(QUEEN_PID))]
KING_MD = [("x-hivekernel-pid", "1")]

RESEARCH_TOPIC = "How AI agents will change software development in 2026"
NUM_WORKERS = 3


# ============================================================
# .env loader (no python-dotenv dependency)
# ============================================================

def load_dotenv(path: str = "") -> None:
    """Load .env file into os.environ. Skips comments and blank lines."""
    if not path:
        path = os.path.join(os.path.dirname(__file__), "..", "..", "..", ".env")
    path = os.path.abspath(path)
    if not os.path.isfile(path):
        return
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            if "=" in line:
                key, _, value = line.partition("=")
                os.environ.setdefault(key.strip(), value.strip())


# ============================================================
# Agent definitions
# ============================================================

class LLMResearchLead(LLMAgent):
    """Lead agent: uses LLM to break topic, spawn workers, synthesize."""

    async def handle_task(self, task, ctx: SyscallContext) -> TaskResult:
        topic = task.params.get("topic", "Unknown Topic")

        await ctx.log("info", f"LLMResearchLead starting: '{topic}'")
        await ctx.report_progress("Asking LLM to break down topic...", 10.0)

        # 1. Ask LLM to generate subtopics.
        subtopics_raw = await self.ask(
            f"Break down the following research topic into exactly {NUM_WORKERS} "
            f"distinct subtopics. Return ONLY a JSON array of strings, no other text.\n\n"
            f"Topic: {topic}",
            max_tokens=512,
            temperature=0.5,
        )

        # Parse subtopics (strip markdown fences if present).
        subtopics_text = subtopics_raw.strip()
        if subtopics_text.startswith("```"):
            lines = subtopics_text.split("\n")
            subtopics_text = "\n".join(
                l for l in lines if not l.strip().startswith("```")
            )
        try:
            subtopics = json.loads(subtopics_text)
        except json.JSONDecodeError:
            # Fallback: split by newlines if JSON parse fails.
            subtopics = [
                s.strip().strip('"').strip("- ")
                for s in subtopics_raw.strip().split("\n")
                if s.strip() and not s.strip().startswith("```")
            ][:NUM_WORKERS]

        await ctx.log("info", f"LLM generated {len(subtopics)} subtopics")
        for i, s in enumerate(subtopics):
            await ctx.log("info", f"  Subtopic {i+1}: {s}")

        await ctx.report_progress("Spawning research workers...", 20.0)

        # 2. Spawn workers.
        worker_pids = []
        for i in range(len(subtopics)):
            pid = await ctx.spawn(
                name=f"llm-researcher-{i}",
                role="task",
                cognitive_tier="operational",
                model="mini",
                system_prompt=f"You are a research assistant specializing in: {subtopics[i]}",
                runtime_image="examples.llm_research_team:LLMResearchWorker",
                runtime_type="python",
            )
            worker_pids.append(pid)
            await ctx.log("info", f"Spawned llm-researcher-{i} as PID {pid}")

        await ctx.report_progress("Workers ready, delegating research...", 30.0)

        # 3. Delegate subtopics to workers.
        findings = []
        for i, subtopic in enumerate(subtopics):
            target_pid = worker_pids[i]
            pct = 30.0 + (40.0 * (i + 1) / len(subtopics))
            await ctx.report_progress(f"Worker {i} researching: {subtopic[:50]}...", pct)

            result = await ctx.execute_on(
                pid=target_pid,
                description=subtopic,
                params={"subtopic": subtopic, "index": str(i)},
            )
            findings.append({
                "subtopic": subtopic,
                "worker_pid": target_pid,
                "finding": result.output,
            })

            # Store individual finding.
            safe_key = subtopic.lower().replace(" ", "-")[:40]
            await ctx.store_artifact(
                key=f"llm-finding-{safe_key}",
                content=result.output.encode("utf-8"),
                content_type="text/plain",
            )

        await ctx.report_progress("All research collected, synthesizing...", 75.0)

        # 4. Ask LLM to synthesize findings into a report.
        findings_text = "\n\n".join(
            f"## {f['subtopic']}\n{f['finding']}" for f in findings
        )
        report = await self.ask(
            f"You have research findings on the topic: '{topic}'.\n\n"
            f"Individual findings:\n\n{findings_text}\n\n"
            f"Synthesize these into a cohesive research report with an executive summary, "
            f"key insights, and conclusions. Keep it concise (under 500 words).",
            max_tokens=2048,
            temperature=0.5,
        )

        await ctx.log("info", f"LLM synthesized report ({len(report)} chars)")

        # 5. Store final report.
        report_data = {
            "topic": topic,
            "subtopics": subtopics,
            "findings": findings,
            "synthesized_report": report,
            "total_tokens": self.llm.total_tokens,
        }
        report_json = json.dumps(report_data, indent=2)
        artifact_id = await ctx.store_artifact(
            key="llm-research-report-final",
            content=report_json.encode("utf-8"),
            content_type="application/json",
        )
        await ctx.report_progress("Report stored", 90.0)

        # 6. Kill workers.
        for pid in worker_pids:
            await ctx.kill(pid)
        await ctx.log("info", f"All {len(worker_pids)} workers terminated")

        # Collect total tokens from workers (stored in findings metadata).
        total_tokens = self.llm.total_tokens
        for f in findings:
            total_tokens += int(
                f.get("metadata", {}).get("tokens_used", "0")
                if isinstance(f.get("metadata"), dict) else 0
            )

        return TaskResult(
            exit_code=0,
            output=report,
            artifacts={
                "report_json": report_json,
                "artifact_id": artifact_id,
                "worker_count": str(len(worker_pids)),
                "total_tokens": str(total_tokens),
            },
            metadata={
                "topic": topic,
                "subtopics_count": str(len(subtopics)),
            },
        )


class LLMResearchWorker(LLMAgent):
    """Worker agent: researches a subtopic using LLM."""

    async def handle_task(self, task, ctx: SyscallContext) -> TaskResult:
        subtopic = task.params.get("subtopic", task.description)
        index = task.params.get("index", "0")

        await ctx.report_progress(f"Researching: {subtopic[:50]}...", 20.0)
        await ctx.log("info", f"Worker PID {self.pid} researching: {subtopic}")

        # Ask LLM for research findings.
        finding = await self.ask(
            f"Research the following subtopic and provide key findings, "
            f"trends, and notable developments. Be specific and concise (200-300 words).\n\n"
            f"Subtopic: {subtopic}",
            max_tokens=1024,
            temperature=0.7,
        )

        tokens_used = self.llm.total_tokens
        await ctx.log("info", f"Worker PID {self.pid} done, {tokens_used} tokens used")
        await ctx.report_progress("Research complete", 100.0)

        return TaskResult(
            exit_code=0,
            output=finding,
            artifacts={"subtopic": subtopic},
            metadata={"worker_pid": str(self.pid), "tokens_used": str(tokens_used)},
        )


# ============================================================
# Demo runner (same pattern as demo_showcase.py)
# ============================================================

ROLE_NAMES = {0: "kernel", 1: "daemon", 2: "agent", 3: "architect", 4: "lead", 5: "worker", 6: "task"}
TIER_NAMES = {0: "strategic", 1: "tactical", 2: "operational"}
STATE_NAMES = {0: "idle", 1: "running", 2: "blocked", 3: "sleeping", 4: "dead", 5: "zombie"}


def fmt_role(v):
    return ROLE_NAMES.get(v, "?")


def fmt_tier(v):
    return TIER_NAMES.get(v, "?")


def fmt_state(v):
    return STATE_NAMES.get(v, "?")


async def wait_for_kernel(addr, timeout=15.0):
    """Poll kernel gRPC until it responds."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            channel = grpc.aio.insecure_channel(addr)
            stub = core_pb2_grpc.CoreServiceStub(channel)
            await stub.GetProcessInfo(
                core_pb2.ProcessInfoRequest(pid=1),
                metadata=KING_MD,
                timeout=2,
            )
            await channel.close()
            return
        except Exception:
            await asyncio.sleep(0.3)
    raise TimeoutError(f"Kernel at {addr} not ready after {timeout}s")


async def print_process_table(stub):
    """Print a ps-like process table."""
    children = await stub.ListChildren(
        core_pb2.ListChildrenRequest(recursive=True),
        metadata=KING_MD,
    )
    king_info = await stub.GetProcessInfo(
        core_pb2.ProcessInfoRequest(pid=1), metadata=KING_MD
    )

    print("  PID  PPID  ROLE        TIER          STATE     NAME")
    print("  ---  ----  ----------  ------------  --------  ----")
    print(
        f"  {king_info.pid:<4} {king_info.ppid:<5} "
        f"{fmt_role(king_info.role):<10}  {fmt_tier(king_info.cognitive_tier):<12}  "
        f"{fmt_state(king_info.state):<8}  {king_info.name}"
    )
    for c in children.children:
        print(
            f"  {c.pid:<4} {c.ppid:<5} "
            f"{fmt_role(c.role):<10}  {fmt_tier(c.cognitive_tier):<12}  "
            f"{fmt_state(c.state):<8}  {c.name}"
        )


def pause(msg: str = "Press Enter to continue..."):
    """Block until user presses Enter (runs in thread to not block event loop)."""
    input(f"\n  >>> {msg}")


async def async_pause(msg: str = "Press Enter to continue..."):
    """Non-blocking pause for use inside async code."""
    await asyncio.to_thread(pause, msg)


async def run_demo(with_dashboard: bool = False):
    """Main demo flow with real LLM calls."""
    channel = grpc.aio.insecure_channel(CORE_ADDR)
    stub = core_pb2_grpc.CoreServiceStub(channel)
    lead_pid = None

    try:
        # --- 1. Initial process tree ---
        print("\n--- 1. Process Tree (initial) ---")
        await print_process_table(stub)

        if with_dashboard:
            await async_pause("Check the dashboard in browser. Press Enter to spawn Lead agent...")

        # --- 2. Spawn LLMResearchLead ---
        print("\n--- 2. Spawning LLM Research Lead ---")
        spawn_resp = await stub.SpawnChild(
            agent_pb2.SpawnRequest(
                name="llm-research-lead",
                role=agent_pb2.ROLE_LEAD,
                cognitive_tier=agent_pb2.COG_TACTICAL,
                model="sonnet",
                system_prompt="You are a research team lead. You coordinate research tasks and synthesize findings into reports.",
                runtime_type=agent_pb2.RUNTIME_PYTHON,
                runtime_image="examples.llm_research_team:LLMResearchLead",
            ),
            metadata=QUEEN_MD,
        )
        if not spawn_resp.success:
            print(f"  [FAIL] Could not spawn lead: {spawn_resp.error}")
            return False
        lead_pid = spawn_resp.child_pid
        print(f"  [OK] llm-research-lead spawned (PID {lead_pid})")
        await asyncio.sleep(0.5)

        # --- 3. Process tree with lead ---
        print("\n--- 3. Process Tree (with lead) ---")
        await print_process_table(stub)

        if with_dashboard:
            await async_pause("Lead agent visible on dashboard. Press Enter to start LLM research...")

        # --- 4. Execute LLM research task ---
        print(f'\n--- 4. LLM Research Task: "{RESEARCH_TOPIC}" ---')
        print("  (This calls real LLMs -- may take 30-60 seconds...)")
        print("  (Watch the dashboard -- workers will appear as they spawn!)")

        exec_resp = await stub.ExecuteTask(
            core_pb2.ExecuteTaskRequest(
                target_pid=lead_pid,
                description=f"Research: {RESEARCH_TOPIC}",
                params={"topic": RESEARCH_TOPIC},
                timeout_seconds=300,
            ),
            metadata=KING_MD,
            timeout=360,
        )

        if not exec_resp.success:
            print(f"  [FAIL] Task failed: {exec_resp.error}")
            return False

        result = exec_resp.result
        print("  [OK] Research complete!")

        # --- 5. Show artifacts ---
        print("\n--- 5. Artifacts ---")
        try:
            arts = await stub.ListArtifacts(
                core_pb2.ListArtifactsRequest(prefix=""),
                metadata=QUEEN_MD,
            )
            for art in arts.artifacts:
                size_str = f"{art.size_bytes}B" if art.size_bytes < 1024 else f"{art.size_bytes // 1024}KB"
                print(f"  {art.key:<40} {art.content_type:<20} {size_str}")
        except Exception as e:
            print(f"  (could not list artifacts: {e})")

        # --- 6. Show synthesized report ---
        print("\n--- 6. Synthesized Report (LLM-generated) ---")
        print("-" * 60)
        print(result.output)
        print("-" * 60)

        # --- 7. Token usage ---
        total_tokens = result.artifacts.get("total_tokens", "?")
        worker_count = result.artifacts.get("worker_count", "?")
        print(f"\n--- 7. Stats ---")
        print(f"  Workers spawned: {worker_count}")
        print(f"  Total tokens used: {total_tokens}")

        # --- 8. Final process tree ---
        print("\n--- 8. Process Tree (after cleanup) ---")
        await print_process_table(stub)

        if with_dashboard:
            await async_pause("Check zombie workers on dashboard. Press Enter to cleanup...")

        # --- 9. Cleanup lead ---
        print("\n--- 9. Cleanup ---")
        if lead_pid:
            try:
                kill_resp = await stub.KillChild(
                    agent_pb2.KillRequest(target_pid=lead_pid, recursive=True),
                    metadata=QUEEN_MD,
                )
                killed = list(kill_resp.killed_pids)
                print(f"  [OK] Killed lead + children: PIDs {killed}")
            except Exception as e:
                print(f"  (cleanup note: {e})")

        if with_dashboard:
            await async_pause("All done! Press Enter to shut down...")

        print(f"\n  [OK] Done! Real LLM research completed successfully.")
        return True

    finally:
        await channel.close()


def main():
    print("=" * 60)
    print("  HiveKernel Demo: LLM Research Team (Real AI)")
    print("=" * 60)

    # Parse --no-dashboard flag.
    with_dashboard = "--no-dashboard" not in sys.argv

    # Load .env for API key.
    load_dotenv()

    api_key = os.environ.get("OPENROUTER_API_KEY", "")
    if not api_key:
        print("\nFATAL: OPENROUTER_API_KEY not set")
        print("Create a .env file in the project root:")
        print("  OPENROUTER_API_KEY=sk-or-v1-your-key-here")
        print("\nOr set the environment variable directly.")
        sys.exit(1)
    print(f"\n  API key: {api_key[:12]}...{api_key[-4:]}")

    # Check kernel binary exists.
    if not os.path.isfile(KERNEL_BIN):
        print(f"\nFATAL: Kernel binary not found: {KERNEL_BIN}")
        print("Build it first: go build -o bin/hivekernel.exe ./cmd/hivekernel")
        sys.exit(1)

    # Set up PYTHONPATH so spawned agents can find the SDK.
    env = os.environ.copy()
    pythonpath = env.get("PYTHONPATH", "")
    if SDK_DIR not in pythonpath:
        env["PYTHONPATH"] = SDK_DIR + os.pathsep + pythonpath if pythonpath else SDK_DIR

    # --- Start kernel ---
    print("\n--- 0. Starting Kernel ---")
    kernel_proc = subprocess.Popen(
        [KERNEL_BIN, "--listen", ":50051"],
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    print(f"  [OK] Kernel started (OS PID {kernel_proc.pid})")

    # --- Start dashboard ---
    dashboard_proc = None
    if with_dashboard:
        if not os.path.isfile(DASHBOARD_APP):
            print(f"  [WARN] Dashboard not found: {DASHBOARD_APP}")
            print("  Running without dashboard.")
            with_dashboard = False
        else:
            try:
                dashboard_proc = subprocess.Popen(
                    [sys.executable, DASHBOARD_APP],
                    env=env,
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                )
                print(f"  [OK] Dashboard started (OS PID {dashboard_proc.pid})")
                print(f"  [OK] Open http://localhost:{DASHBOARD_PORT} in your browser")
            except Exception as e:
                print(f"  [WARN] Could not start dashboard: {e}")
                print("  Running without dashboard.")
                with_dashboard = False

    try:
        asyncio.run(_wait_and_run(kernel_proc, with_dashboard))
    finally:
        # Stop dashboard.
        if dashboard_proc is not None:
            print("\n--- Stopping Dashboard ---")
            dashboard_proc.terminate()
            try:
                dashboard_proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                dashboard_proc.kill()
                dashboard_proc.wait()
            print("  [OK] Dashboard stopped")

        # Stop kernel.
        print("\n--- Stopping Kernel ---")
        kernel_proc.terminate()
        try:
            kernel_proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            kernel_proc.kill()
            kernel_proc.wait()
        print("  [OK] Kernel stopped")
        print("=" * 60)


async def _wait_and_run(kernel_proc, with_dashboard: bool = False):
    print("  Waiting for gRPC readiness...")
    await wait_for_kernel(CORE_ADDR, timeout=15)
    print("  [OK] Kernel ready")

    # Give dashboard a moment to start, then open browser.
    if with_dashboard:
        await asyncio.sleep(2)
        url = f"http://localhost:{DASHBOARD_PORT}"
        print(f"\n  Opening dashboard: {url}")
        webbrowser.open(url)

    success = await run_demo(with_dashboard=with_dashboard)
    if not success:
        sys.exit(1)


if __name__ == "__main__":
    main()
