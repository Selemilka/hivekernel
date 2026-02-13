"""
HiveKernel Demo: AI Research Team
==================================

One-command showcase of the full HiveKernel multi-agent stack:
  1. Starts the Go kernel (bin/hivekernel.exe)
  2. Spawns a ResearchLead agent (real Python process)
  3. ResearchLead spawns 3 ResearchWorker agents (real Python processes)
  4. ResearchLead delegates research subtopics to workers via execute_on
  5. Workers "research" their subtopics and return findings
  6. ResearchLead stores individual + merged report as artifacts
  7. ResearchLead kills workers and returns summary
  8. Script prints process table, artifacts, and results

Usage:
    python sdk/python/examples/demo_showcase.py
"""

import asyncio
import json
import os
import subprocess
import sys
import time

SDK_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, SDK_DIR)

import grpc.aio  # noqa: E402
from hivekernel_sdk import agent_pb2, agent_pb2_grpc, core_pb2, core_pb2_grpc  # noqa: E402
from hivekernel_sdk import HiveAgent, TaskResult, AgentConfig  # noqa: E402
from hivekernel_sdk.syscall import SyscallContext  # noqa: E402

KERNEL_BIN = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "..", "..", "bin", "hivekernel.exe")
)
CORE_ADDR = "localhost:50051"
QUEEN_PID = 2
QUEEN_MD = [("x-hivekernel-pid", str(QUEEN_PID))]
KING_MD = [("x-hivekernel-pid", "1")]

# ============================================================
# Agent definitions (used as runtime_image by the kernel)
# ============================================================

RESEARCH_TOPIC = "Artificial Intelligence in 2026"
SUBTOPICS = [
    "LLM Agents and Autonomous Systems",
    "Multimodal AI and Vision-Language Models",
    "AI Safety and Alignment Research",
]


class ResearchLead(HiveAgent):
    """Lead agent: spawns workers, delegates subtopics, merges results."""

    async def on_init(self, config: AgentConfig) -> None:
        pass

    async def handle_task(self, task, ctx: SyscallContext) -> TaskResult:
        topic = task.params.get("topic", "Unknown Topic")
        subtopics_raw = task.params.get("subtopics", "[]")
        subtopics = json.loads(subtopics_raw)
        worker_count = len(subtopics)

        await ctx.log("info", f"ResearchLead starting: topic='{topic}', {worker_count} subtopics")
        await ctx.report_progress("Spawning workers...", 20.0)

        # 1. Spawn research workers.
        worker_pids = []
        for i in range(worker_count):
            pid = await ctx.spawn(
                name=f"researcher-{i}",
                role="task",
                cognitive_tier="operational",
                model="mini",
                runtime_image="examples.demo_showcase:ResearchWorker",
                runtime_type="python",
            )
            worker_pids.append(pid)
            await ctx.log("info", f"Spawned researcher-{i} as PID {pid}")

        await ctx.report_progress("Workers ready", 30.0)

        # 2. Delegate subtopics to workers.
        findings = []
        for i, subtopic in enumerate(subtopics):
            target_pid = worker_pids[i]
            pct = 40.0 + (40.0 * (i + 1) / len(subtopics))
            await ctx.report_progress(f'Delegating "{subtopic}" to PID {target_pid}', pct)

            result = await ctx.execute_on(
                pid=target_pid,
                description=subtopic,
                params={"topic": subtopic, "index": str(i)},
            )
            findings.append({
                "subtopic": subtopic,
                "worker_pid": target_pid,
                "finding": result.output,
            })

            # Store individual finding as artifact.
            safe_key = subtopic.lower().replace(" ", "-")[:40]
            await ctx.store_artifact(
                key=f"finding-{safe_key}",
                content=result.output.encode("utf-8"),
                content_type="text/plain",
            )

        await ctx.report_progress("All research collected", 85.0)

        # 3. Merge and store final report.
        report = {
            "topic": topic,
            "subtopics_count": len(subtopics),
            "findings": findings,
        }
        report_json = json.dumps(report, indent=2)
        artifact_id = await ctx.store_artifact(
            key="research-report-final",
            content=report_json.encode("utf-8"),
            content_type="application/json",
        )
        await ctx.report_progress("Report stored", 90.0)

        # 4. Kill workers.
        for pid in worker_pids:
            await ctx.kill(pid)
        await ctx.log("info", f"All {worker_count} workers terminated")

        await ctx.report_progress("Cleanup done", 95.0)

        return TaskResult(
            exit_code=0,
            output=f"Completed {len(subtopics)} subtopics with {worker_count} workers",
            artifacts={
                "report": report_json,
                "artifact_id": artifact_id,
                "worker_count": str(worker_count),
            },
            metadata={
                "topic": topic,
                "subtopics_count": str(len(subtopics)),
            },
        )


class ResearchWorker(HiveAgent):
    """Worker agent: receives a subtopic, produces research findings."""

    async def on_init(self, config: AgentConfig) -> None:
        pass

    async def handle_task(self, task, ctx: SyscallContext) -> TaskResult:
        subtopic = task.params.get("topic", task.description)
        index = task.params.get("index", "0")

        await ctx.report_progress(f"Researching: {subtopic}", 30.0)
        await ctx.log("info", f"Worker PID {self.pid} researching: {subtopic}")

        # Simulate research work (brief pause).
        await asyncio.sleep(0.3)

        # Generate findings (in a real system this would call an LLM).
        finding = (
            f"Research findings for '{subtopic}': "
            f"This area shows significant progress in 2026. "
            f"Key developments include improved architectures, "
            f"better benchmarks, and wider adoption in industry. "
            f"[Worker PID {self.pid}, subtopic #{index}]"
        )

        await ctx.report_progress("Research complete", 100.0)

        return TaskResult(
            exit_code=0,
            output=finding,
            artifacts={"subtopic": subtopic},
            metadata={"worker_pid": str(self.pid)},
        )


# ============================================================
# Demo runner
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
    # Get king's children recursively (from king's perspective).
    children = await stub.ListChildren(
        core_pb2.ListChildrenRequest(recursive=True),
        metadata=KING_MD,
    )
    # Also get king's own info.
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


async def run_demo():
    """Main demo flow."""
    channel = grpc.aio.insecure_channel(CORE_ADDR)
    stub = core_pb2_grpc.CoreServiceStub(channel)
    lead_pid = None

    try:
        # --- 1. Initial process tree ---
        print("\n--- 1. Process Tree (initial) ---")
        await print_process_table(stub)

        # --- 2. Spawn ResearchLead ---
        print("\n--- 2. Spawning Research Lead ---")
        spawn_resp = await stub.SpawnChild(
            agent_pb2.SpawnRequest(
                name="research-lead",
                role=agent_pb2.ROLE_LEAD,
                cognitive_tier=agent_pb2.COG_TACTICAL,
                model="sonnet",
                runtime_type=agent_pb2.RUNTIME_PYTHON,
                runtime_image="examples.demo_showcase:ResearchLead",
            ),
            metadata=QUEEN_MD,
        )
        if not spawn_resp.success:
            print(f"  [FAIL] Could not spawn lead: {spawn_resp.error}")
            return False
        lead_pid = spawn_resp.child_pid
        print(f"  [OK] research-lead spawned (PID {lead_pid})")
        await asyncio.sleep(0.5)

        # --- 3. Updated process tree ---
        print("\n--- 3. Process Tree (with lead) ---")
        await print_process_table(stub)

        # --- 4. Execute research task ---
        print(f'\n--- 4. Research Task: "{RESEARCH_TOPIC}" ---')
        subtopics_json = json.dumps(SUBTOPICS)

        exec_resp = await stub.ExecuteTask(
            core_pb2.ExecuteTaskRequest(
                target_pid=lead_pid,
                description=f"Research: {RESEARCH_TOPIC}",
                params={
                    "topic": RESEARCH_TOPIC,
                    "subtopics": subtopics_json,
                },
                timeout_seconds=120,
            ),
            metadata=KING_MD,
            timeout=180,
        )

        if not exec_resp.success:
            print(f"  [FAIL] Task failed: {exec_resp.error}")
            return False

        result = exec_resp.result
        print(f"  [OK] {result.output}")

        # --- 5. Show artifacts ---
        print("\n--- 5. Artifacts ---")
        try:
            arts = await stub.ListArtifacts(
                core_pb2.ListArtifactsRequest(prefix=""),
                metadata=QUEEN_MD,
            )
            for art in arts.artifacts:
                size_str = f"{art.size_bytes}B" if art.size_bytes < 1024 else f"{art.size_bytes // 1024}KB"
                print(f"  {art.key:<35} {art.content_type:<20} {size_str}")
        except Exception as e:
            print(f"  (could not list artifacts: {e})")

        # --- 6. Show report excerpt ---
        print("\n--- 6. Report Summary ---")
        report_raw = result.artifacts.get("report", "{}")
        try:
            report = json.loads(report_raw)
            print(f"  Topic: {report.get('topic', '?')}")
            print(f"  Subtopics researched: {report.get('subtopics_count', '?')}")
            for f in report.get("findings", []):
                finding_short = f["finding"][:80] + "..." if len(f["finding"]) > 80 else f["finding"]
                print(f"    - [{f['worker_pid']}] {f['subtopic']}")
                print(f"      {finding_short}")
        except Exception:
            print(f"  (raw): {report_raw[:200]}")

        # --- 7. Final process tree ---
        print("\n--- 7. Process Tree (after cleanup) ---")
        await print_process_table(stub)

        # --- 8. Cleanup ---
        print("\n--- 8. Cleanup ---")
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

        worker_count = int(result.artifacts.get("worker_count", "0"))
        artifact_count = len(arts.artifacts) if arts else 0
        print(
            f"\n  [OK] Done! {worker_count + 1} agents spawned, "
            f"{len(SUBTOPICS)} tasks delegated, "
            f"{artifact_count} artifacts stored"
        )
        return True

    finally:
        await channel.close()


def main():
    print("=" * 60)
    print("  HiveKernel Demo: AI Research Team")
    print("=" * 60)

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

    try:
        asyncio.run(_wait_and_run(kernel_proc))
    finally:
        print("\n--- Stopping Kernel ---")
        kernel_proc.terminate()
        try:
            kernel_proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            kernel_proc.kill()
            kernel_proc.wait()
        print("  [OK] Kernel stopped")
        print("=" * 60)


async def _wait_and_run(kernel_proc):
    print("  Waiting for gRPC readiness...")
    await wait_for_kernel(CORE_ADDR, timeout=15)
    print("  [OK] Kernel ready")
    success = await run_demo()
    if not success:
        sys.exit(1)


if __name__ == "__main__":
    main()
