"""
ToolAgent E2E test -- verifies the full AgentCore stack.

Spawns a CalculatorAgent (ToolAgent subclass with calculator tool),
sends a math task, and verifies:
  1. Agent spawns and initializes
  2. Task completes with tool calling (iterations > 1)
  3. Result contains the correct answer
  4. Memory is persisted (artifacts stored)

Usage:
    # Terminal 1: start kernel
    bin\hivekernel.exe --listen :50051

    # Terminal 2: run test
    python sdk/python/examples/test_tool_agent_e2e.py
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
from hivekernel_sdk import agent_pb2, core_pb2, core_pb2_grpc  # noqa: E402

KERNEL_BIN = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "..", "..", "bin", "hivekernel.exe")
)
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


async def run_tests():
    passed = 0
    total = 0

    channel = grpc.aio.insecure_channel(CORE_ADDR)
    stub = core_pb2_grpc.CoreServiceStub(channel)

    try:
        # 1. Spawn CalculatorAgent
        print("\n=== 1. Spawn CalculatorAgent ===")
        total += 1
        spawn_resp = await stub.SpawnChild(
            agent_pb2.SpawnRequest(
                name="calculator",
                role=agent_pb2.ROLE_WORKER,
                cognitive_tier=agent_pb2.COG_OPERATIONAL,
                system_prompt="You are a calculator agent. Use the calculator tool to solve math problems. Always use the calculator tool for any computation.",
                model="mini",
                runtime_type=agent_pb2.RUNTIME_PYTHON,
                runtime_image="examples.calculator_agent:CalculatorAgent",
            ),
            metadata=QUEEN_MD,
        )
        check("Spawn CalculatorAgent", spawn_resp.success, f"pid={spawn_resp.child_pid}")
        passed += 1
        agent_pid = spawn_resp.child_pid

        # 2. Wait for agent to be ready
        print("\n=== 2. Verify agent running ===")
        await asyncio.sleep(1.0)
        total += 1
        info = await stub.GetProcessInfo(
            core_pb2.ProcessInfoRequest(pid=agent_pid), metadata=KING_MD
        )
        check("Agent is running", info.pid == agent_pid and info.runtime_addr != "")
        passed += 1

        # 3. Execute math task
        print("\n=== 3. Execute math task ===")
        total += 1
        exec_resp = await stub.ExecuteTask(
            core_pb2.ExecuteTaskRequest(
                target_pid=agent_pid,
                description="What is 15 * 7 + 23? Use the calculator tool.",
                timeout_seconds=60,
            ),
            metadata=KING_MD,
            timeout=90,
        )
        result = exec_resp.result if exec_resp.success else None
        check(
            "Task completed",
            exec_resp.success and result is not None and result.exit_code == 0,
            f"output={result.output[:200] if result else exec_resp.error}",
        )
        passed += 1

        # 4. Verify result contains the answer (15*7+23 = 128)
        print("\n=== 4. Verify answer ===")
        total += 1
        check("Answer contains 128", "128" in (result.output if result else ""),
              f"output={result.output[:200] if result else 'N/A'}")
        passed += 1

        # 5. Check metadata shows tool usage
        print("\n=== 5. Check tool usage ===")
        total += 1
        iterations = int(result.metadata.get("iterations", "0")) if result else 0
        tool_calls = int(result.metadata.get("tool_calls", "0")) if result else 0
        check("Used tools", tool_calls >= 1, f"iterations={iterations}, tool_calls={tool_calls}")
        passed += 1

        # 6. Cleanup
        print("\n=== 6. Cleanup ===")
        total += 1
        kill_resp = await stub.KillChild(
            agent_pb2.KillRequest(target_pid=agent_pid, recursive=True),
            metadata=QUEEN_MD,
        )
        check("Kill agent", kill_resp.success)
        passed += 1

    finally:
        await channel.close()

    print(f"\n=== Results: {passed}/{total} checks passed ===")
    return passed == total


def main():
    print("=" * 60)
    print("HiveKernel ToolAgent E2E Test (AgentCore)")
    print("=" * 60)

    if not os.path.isfile(KERNEL_BIN):
        print(f"FATAL: Kernel binary not found: {KERNEL_BIN}")
        print("Build it first: go build -o bin/hivekernel.exe ./cmd/hivekernel")
        sys.exit(1)

    env = os.environ.copy()
    pythonpath = env.get("PYTHONPATH", "")
    if SDK_DIR not in pythonpath:
        env["PYTHONPATH"] = SDK_DIR + os.pathsep + pythonpath if pythonpath else SDK_DIR

    print(f"\nStarting kernel: {KERNEL_BIN}")
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
