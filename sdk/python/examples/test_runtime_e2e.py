"""
End-to-end runtime test: kernel spawns a real Python agent process.

This test:
  1. Starts the HiveKernel binary as a subprocess
  2. Spawns a child with runtime_image (real Python process)
  3. Verifies the child is running (GetProcessInfo)
  4. Connects to the child agent directly and executes a task
  5. Verifies the task result
  6. Kills the child
  7. Stops the kernel

Usage:
    python sdk/python/examples/test_runtime_e2e.py
"""

import asyncio
import os
import subprocess
import sys
import time

# Ensure sdk/python is in PYTHONPATH so hivekernel_sdk is importable.
SDK_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, SDK_DIR)

import grpc.aio  # noqa: E402
from hivekernel_sdk import agent_pb2, agent_pb2_grpc, core_pb2, core_pb2_grpc  # noqa: E402

KERNEL_BIN = os.path.join(os.path.dirname(__file__), "..", "..", "..", "bin", "hivekernel.exe")
KERNEL_BIN = os.path.abspath(KERNEL_BIN)
CORE_ADDR = "localhost:50051"

# Use queen PID=2 as the parent (king auto-spawns queen in demo).
QUEEN_PID = 2
QUEEN_MD = [("x-hivekernel-pid", str(QUEEN_PID))]
KING_MD = [("x-hivekernel-pid", "1")]


async def wait_for_kernel(addr: str, timeout: float = 10.0):
    """Wait until the kernel gRPC server is reachable."""
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
    """Print a check result."""
    status = "OK" if ok else "FAIL"
    msg = f"  [{status}] {label}"
    if detail:
        msg += f" -- {detail}"
    print(msg)
    if not ok:
        raise AssertionError(f"Check failed: {label} {detail}")


async def run_tests():
    passed = 0
    total = 0

    # --- Connect to kernel ---
    channel = grpc.aio.insecure_channel(CORE_ADDR)
    stub = core_pb2_grpc.CoreServiceStub(channel)

    try:
        # 1. Verify kernel is running.
        print("\n=== 1. Kernel connectivity ===")
        info = await stub.GetProcessInfo(
            core_pb2.ProcessInfoRequest(pid=1), metadata=KING_MD
        )
        total += 1
        check("Kernel PID 1 exists", info.pid == 1, f"name={info.name}")
        passed += 1

        # 2. Spawn a real Python agent (sub_worker).
        print("\n=== 2. Spawn real Python agent ===")
        spawn_resp = await stub.SpawnChild(
            agent_pb2.SpawnRequest(
                name="e2e-sub-worker",
                role=agent_pb2.ROLE_WORKER,
                cognitive_tier=agent_pb2.COG_OPERATIONAL,
                model="test",
                runtime_type=agent_pb2.RUNTIME_PYTHON,
                runtime_image="examples.sub_worker:SubWorker",
            ),
            metadata=QUEEN_MD,
        )
        total += 1
        check(
            "SpawnChild succeeded",
            spawn_resp.success,
            f"child_pid={spawn_resp.child_pid}, error={spawn_resp.error}",
        )
        passed += 1
        child_pid = spawn_resp.child_pid

        # 3. Verify child process info.
        print("\n=== 3. Verify child process ===")
        await asyncio.sleep(0.5)  # Give agent a moment to fully initialize.
        child_info = await stub.GetProcessInfo(
            core_pb2.ProcessInfoRequest(pid=child_pid), metadata=KING_MD
        )
        total += 1
        check("Child exists", child_info.pid == child_pid, f"name={child_info.name}")
        passed += 1

        total += 1
        # After spawn + Init, process is Idle (not yet executing a task).
        check(
            "Child state is Idle",
            child_info.state == agent_pb2.STATE_IDLE,
            f"state={child_info.state}",
        )
        passed += 1

        total += 1
        agent_addr = child_info.runtime_addr
        check(
            "Runtime addr is set",
            agent_addr != "" and not agent_addr.startswith("virtual://"),
            f"addr={agent_addr}",
        )
        passed += 1

        # 4. Connect to the child agent directly and execute a task.
        print("\n=== 4. Execute task on spawned agent ===")
        agent_channel = grpc.aio.insecure_channel(agent_addr)
        agent_stub = agent_pb2_grpc.AgentServiceStub(agent_channel)

        # 4a. Heartbeat check.
        total += 1
        hb = await agent_stub.Heartbeat(agent_pb2.HeartbeatRequest(), timeout=5)
        check("Heartbeat OK", hb.state == agent_pb2.STATE_RUNNING)
        passed += 1

        # 4b. Open Execute stream, send task, get result.
        total += 1
        task_result = await execute_task(
            agent_stub,
            task_id="e2e-task-1",
            description="hello from E2E test",
        )
        check(
            "Task completed",
            task_result is not None and task_result.exit_code == 0,
            f"output={task_result.output if task_result else 'None'}",
        )
        passed += 1

        total += 1
        check(
            "Task output correct",
            "sub-worker done: hello from E2E test" in task_result.output,
            f"output={task_result.output}",
        )
        passed += 1

        await agent_channel.close()

        # 5. Kill the child.
        print("\n=== 5. Kill child agent ===")
        total += 1
        kill_resp = await stub.KillChild(
            agent_pb2.KillRequest(target_pid=child_pid, recursive=True),
            metadata=QUEEN_MD,
        )
        check("Kill succeeded", kill_resp.success, f"killed={list(kill_resp.killed_pids)}")
        passed += 1

        # 6. Verify child is dead.
        await asyncio.sleep(0.5)
        total += 1
        dead_info = await stub.GetProcessInfo(
            core_pb2.ProcessInfoRequest(pid=child_pid), metadata=KING_MD
        )
        check(
            "Child is dead",
            dead_info.state == agent_pb2.STATE_DEAD,
            f"state={dead_info.state}",
        )
        passed += 1

    finally:
        await channel.close()

    print(f"\n=== Results: {passed}/{total} checks passed ===")
    return passed == total


async def execute_task(
    agent_stub: agent_pb2_grpc.AgentServiceStub,
    task_id: str,
    description: str,
    params: dict[str, str] | None = None,
    timeout: float = 10.0,
) -> agent_pb2.TaskResult | None:
    """Open an Execute bidi stream, send a task, collect result."""
    result_holder = {}

    async def request_gen():
        # Send the task request.
        yield agent_pb2.ExecuteInput(
            task=agent_pb2.TaskRequest(
                task_id=task_id,
                description=description,
                params=params or {},
            )
        )
        # Keep the stream open until we get the result.
        # We wait on an event that gets set when we receive the result.
        while "done" not in result_holder:
            await asyncio.sleep(0.1)

    stream = agent_stub.Execute(request_gen(), timeout=timeout)

    async for progress in stream:
        if progress.type == agent_pb2.PROGRESS_COMPLETED:
            result_holder["done"] = True
            return progress.result
        elif progress.type == agent_pb2.PROGRESS_FAILED:
            result_holder["done"] = True
            print(f"    Task failed: {progress.message}")
            return progress.result
        elif progress.type == agent_pb2.PROGRESS_UPDATE:
            print(f"    Progress: {progress.progress_percent}% {progress.message}")

    return None


def main():
    print("=" * 60)
    print("HiveKernel Runtime E2E Test")
    print("=" * 60)

    # Check kernel binary exists.
    if not os.path.isfile(KERNEL_BIN):
        print(f"FATAL: Kernel binary not found: {KERNEL_BIN}")
        print("Build it first: go build -o bin/hivekernel.exe ./cmd/hivekernel")
        sys.exit(1)

    # Set PYTHONPATH so spawned Python agents can find hivekernel_sdk and examples.
    env = os.environ.copy()
    pythonpath = env.get("PYTHONPATH", "")
    if SDK_DIR not in pythonpath:
        env["PYTHONPATH"] = SDK_DIR + os.pathsep + pythonpath if pythonpath else SDK_DIR

    # Start kernel.
    print(f"\nStarting kernel: {KERNEL_BIN}")
    kernel_proc = subprocess.Popen(
        [KERNEL_BIN, "--listen", ":50051"],
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    print(f"Kernel PID (OS): {kernel_proc.pid}")

    try:
        # Wait for kernel to be ready.
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
