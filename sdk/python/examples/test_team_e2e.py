"""
Multi-agent team E2E test.

Tests the full kernel + agent stack:
  1. Kernel spawns a TeamManager agent (real Python process)
  2. TeamManager spawns 2 SubWorker agents (real Python processes)
  3. TeamManager delegates subtasks to workers via execute_on
  4. Workers execute tasks and return results
  5. TeamManager stores results as an artifact
  6. TeamManager kills workers
  7. Test verifies all results

Usage:
    python sdk/python/examples/test_team_e2e.py
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

KERNEL_BIN = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "..", "bin", "hivekernel.exe"))
CORE_ADDR = "localhost:50051"
QUEEN_PID = 2
QUEEN_MD = [("x-hivekernel-pid", str(QUEEN_PID))]
KING_MD = [("x-hivekernel-pid", "1")]


async def wait_for_kernel(addr: str, timeout: float = 15.0):
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


def check(label: str, ok: bool, detail: str = ""):
    status = "OK" if ok else "FAIL"
    msg = f"  [{status}] {label}"
    if detail:
        msg += f" -- {detail}"
    print(msg)
    if not ok:
        raise AssertionError(f"Check failed: {label} {detail}")


async def execute_task(agent_stub, task_id, description, params=None, timeout=30.0):
    """Open Execute bidi stream, send task, collect result."""
    result_holder = {}

    async def request_gen():
        yield agent_pb2.ExecuteInput(
            task=agent_pb2.TaskRequest(
                task_id=task_id,
                description=description,
                params=params or {},
            )
        )
        while "done" not in result_holder:
            await asyncio.sleep(0.1)

    stream = agent_stub.Execute(request_gen(), timeout=timeout)

    async for progress in stream:
        if progress.type == agent_pb2.PROGRESS_COMPLETED:
            result_holder["done"] = True
            return progress.result
        elif progress.type == agent_pb2.PROGRESS_FAILED:
            result_holder["done"] = True
            print(f"    Task FAILED: {progress.message}")
            return progress.result
        elif progress.type == agent_pb2.PROGRESS_UPDATE:
            print(f"    Progress: {progress.progress_percent:.0f}% {progress.message}")

    return None


async def run_tests():
    passed = 0
    total = 0

    channel = grpc.aio.insecure_channel(CORE_ADDR)
    stub = core_pb2_grpc.CoreServiceStub(channel)

    try:
        # 1. Spawn TeamManager (real Python process).
        print("\n=== 1. Spawn TeamManager ===")
        total += 1
        spawn_resp = await stub.SpawnChild(
            agent_pb2.SpawnRequest(
                name="team-manager",
                role=agent_pb2.ROLE_LEAD,
                cognitive_tier=agent_pb2.COG_TACTICAL,
                model="test",
                runtime_type=agent_pb2.RUNTIME_PYTHON,
                runtime_image="examples.team_manager:TeamManager",
            ),
            metadata=QUEEN_MD,
        )
        check("SpawnChild (TeamManager)", spawn_resp.success, f"pid={spawn_resp.child_pid}")
        passed += 1
        manager_pid = spawn_resp.child_pid

        # 2. Verify manager is running.
        print("\n=== 2. Verify TeamManager ===")
        await asyncio.sleep(0.5)
        total += 1
        mgr_info = await stub.GetProcessInfo(
            core_pb2.ProcessInfoRequest(pid=manager_pid), metadata=KING_MD
        )
        check("Manager exists", mgr_info.pid == manager_pid)
        passed += 1

        total += 1
        check("Manager has runtime addr", mgr_info.runtime_addr != "" and not mgr_info.runtime_addr.startswith("virtual://"))
        passed += 1

        # 3. Execute team task via kernel's ExecuteTask RPC.
        # This uses the kernel's executor which handles syscalls from the agent.
        print("\n=== 3. Execute team task ===")
        subtasks = ["build-frontend", "build-backend", "run-tests"]
        total += 1
        exec_resp = await stub.ExecuteTask(
            core_pb2.ExecuteTaskRequest(
                target_pid=manager_pid,
                description="Build and test the project",
                params={
                    "worker_count": "2",
                    "subtasks": json.dumps(subtasks),
                },
                timeout_seconds=60,
            ),
            metadata=KING_MD,
            timeout=90,
        )
        result = exec_resp.result if exec_resp.success else None
        check(
            "Team task completed",
            exec_resp.success and result is not None and result.exit_code == 0,
            f"success={exec_resp.success}, output={result.output if result else exec_resp.error}",
        )
        passed += 1

        # 4. Verify results.
        print("\n=== 4. Verify results ===")
        total += 1
        check(
            "Output mentions 3 subtasks",
            "3 subtasks" in result.output,
            f"output={result.output}",
        )
        passed += 1

        total += 1
        summary_raw = result.artifacts.get("summary", "")
        summary = json.loads(summary_raw) if summary_raw else []
        check("Summary has 3 entries", len(summary) == 3, f"got {len(summary)}")
        passed += 1

        total += 1
        all_success = all(r["exit_code"] == 0 for r in summary)
        check("All subtasks succeeded", all_success)
        passed += 1

        total += 1
        subtask_names = [r["subtask"] for r in summary]
        check("Subtask names match", subtask_names == subtasks, f"got {subtask_names}")
        passed += 1

        total += 1
        for r in summary:
            check(
                f"  Worker output for '{r['subtask']}'",
                "sub-worker done:" in r["output"],
                r["output"],
            )
        passed += 1

        # 5. Verify artifact was stored.
        print("\n=== 5. Verify artifact ===")
        total += 1
        artifact_id = result.artifacts.get("artifact_id", "")
        check("Artifact ID returned", artifact_id != "", f"id={artifact_id}")
        passed += 1

        # Use queen's metadata to fetch artifact (manager PID may already be dead by now).
        total += 1
        try:
            art_resp = await stub.GetArtifact(
                agent_pb2.GetArtifactRequest(artifact_id=artifact_id),
                metadata=QUEEN_MD,
            )
            check("Artifact retrievable", art_resp.found, f"content_type={art_resp.content_type}")
            passed += 1
        except Exception as e:
            check("Artifact retrievable", False, str(e))

        # 6. Check that workers were killed (manager kills them in handle_task).
        print("\n=== 6. Verify workers killed ===")
        total += 1
        children = await stub.ListChildren(
            core_pb2.ListChildrenRequest(recursive=True),
            metadata=[("x-hivekernel-pid", str(manager_pid))],
        )
        alive_children = [c for c in children.children if c.state != agent_pb2.STATE_DEAD]
        check("No alive children of manager", len(alive_children) == 0, f"alive={len(alive_children)}")
        passed += 1

        # 7. Kill manager.
        print("\n=== 7. Cleanup ===")
        total += 1
        kill_resp = await stub.KillChild(
            agent_pb2.KillRequest(target_pid=manager_pid, recursive=True),
            metadata=QUEEN_MD,
        )
        check("Kill manager", kill_resp.success)
        passed += 1

    finally:
        await channel.close()

    print(f"\n=== Results: {passed}/{total} checks passed ===")
    return passed == total


def main():
    print("=" * 60)
    print("HiveKernel Multi-Agent Team E2E Test")
    print("=" * 60)

    if not os.path.isfile(KERNEL_BIN):
        print(f"FATAL: Kernel binary not found: {KERNEL_BIN}")
        sys.exit(1)

    env = os.environ.copy()
    pythonpath = env.get("PYTHONPATH", "")
    if SDK_DIR not in pythonpath:
        env["PYTHONPATH"] = SDK_DIR + os.pathsep + pythonpath if pythonpath else SDK_DIR

    print(f"\nStarting kernel: {KERNEL_BIN}")
    # Use DEVNULL to avoid pipe buffer filling up and blocking the kernel.
    # The kernel produces a lot of log output during multi-agent scenarios.
    kernel_proc = subprocess.Popen(
        [KERNEL_BIN, "--listen", ":50051"],
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    print(f"Kernel PID (OS): {kernel_proc.pid}")

    try:
        print("Waiting for kernel gRPC...")
        asyncio.run(_wait_and_test(kernel_proc))
    finally:
        print("\nStopping kernel...")
        kernel_proc.terminate()
        try:
            kernel_proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            kernel_proc.kill()
            kernel_proc.wait()
        print("Kernel stopped.")


async def _wait_and_test(kernel_proc):
    await wait_for_kernel(CORE_ADDR, timeout=15)
    print("Kernel is ready!")
    success = await run_tests()
    if not success:
        sys.exit(1)


if __name__ == "__main__":
    main()
