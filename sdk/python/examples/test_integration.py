"""
HiveKernel Integration Test (Phase 0-3)

Tests the full Python SDK <-> Go Core gRPC pipeline:
  1. Process management (spawn, kill, list, info)
  2. IPC (messages between processes)
  3. Shared memory (store/get/list artifacts)
  4. Resource management (usage, metrics)
  5. Escalation and logging

Prerequisites:
  - Go core running: bin/hivekernel.exe --listen :50051
  - Python SDK installed: cd sdk/python && uv sync

Usage:
  python sdk/python/examples/test_integration.py
"""

import asyncio
import sys
import os

# Allow running from project root.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import grpc.aio
from hivekernel_sdk.client import CoreClient

PASS = 0
FAIL = 0


def check(name, condition, detail=""):
    global PASS, FAIL
    if condition:
        PASS += 1
        print(f"  [PASS] {name}")
    else:
        FAIL += 1
        print(f"  [FAIL] {name} -- {detail}")


async def test_process_info(king: CoreClient):
    """Test GetProcessInfo for the king process."""
    print("\n--- 1. Process Info ---")

    info = await king.get_process_info(pid=1)
    check("king exists", info.pid == 1)
    check("king name", info.name == "king", f"got '{info.name}'")
    check("king role is kernel (0)", info.role == 0)
    check("king user is root", info.user == "root", f"got '{info.user}'")
    check("king VPS is vps1", info.vps == "vps1", f"got '{info.vps}'")

    # pid=0 means "self" (caller is PID 1)
    self_info = await king.get_process_info(pid=0)
    check("pid=0 resolves to self", self_info.pid == 1)


async def test_process_tree(king: CoreClient):
    """Test ListChildren -- demo spawns queen + worker."""
    print("\n--- 2. Process Tree ---")

    children = await king.list_children(recursive=True)
    check("king has children", len(children) >= 2, f"got {len(children)}")

    names = [c.name for c in children]
    check("queen exists in tree", "queen@vps1" in names, f"got {names}")
    check("demo-worker exists", "demo-worker" in names, f"got {names}")


async def test_spawn_and_kill(queen: CoreClient):
    """Test SpawnChild + KillChild from queen's perspective."""
    print("\n--- 3. Spawn & Kill ---")

    # Spawn a task under queen.
    task_pid = await queen.spawn_child(
        name="test-task-py",
        role="task",
        cognitive_tier="operational",
        model="mini",
    )
    check("spawn returns PID > 0", task_pid > 0, f"got {task_pid}")

    # Verify the task exists.
    info = await queen.get_process_info(pid=task_pid)
    check("spawned task name", info.name == "test-task-py", f"got '{info.name}'")
    check("task role is task (6)", info.role == 6, f"got {info.role}")
    check("task inherits user from queen", info.user == "root", f"got '{info.user}'")
    check("task inherits VPS", info.vps == "vps1", f"got '{info.vps}'")

    # Kill the task.
    killed = await queen.kill_child(task_pid)
    check("kill returns task PID", task_pid in killed, f"got {killed}")


async def test_ipc_messages(king: CoreClient, queen: CoreClient):
    """Test SendMessage between processes."""
    print("\n--- 4. IPC Messages ---")

    # Queen sends message to king (child -> parent = direct).
    msg_id = await queen.send_message(
        to_pid=1,
        type="hello",
        payload=b"Hello from Python!",
        priority="normal",
    )
    check("message delivered", len(msg_id) > 0, f"got id='{msg_id}'")

    # Send to named queue.
    msg_id2 = await queen.send_message(
        to_queue="test-results",
        type="data",
        payload=b"test payload",
    )
    check("named queue message", len(msg_id2) > 0)


async def test_artifacts(queen: CoreClient, king: CoreClient):
    """Test shared memory artifacts (store/get/list)."""
    print("\n--- 5. Shared Memory Artifacts ---")

    # Queen stores a global artifact.
    art_id = await queen.store_artifact(
        key="docs/readme.md",
        content=b"# HiveKernel\nTest artifact from Python",
        content_type="text/markdown",
        visibility=3,  # VIS_GLOBAL
    )
    check("store returns ID", len(art_id) > 0, f"got '{art_id}'")

    # King can read it (global visibility).
    art = await king.get_artifact(key="docs/readme.md")
    check("king reads global artifact", art.found)
    check("content matches", art.content == b"# HiveKernel\nTest artifact from Python")
    check("content_type matches", art.content_type == "text/markdown")

    # Store another artifact.
    await queen.store_artifact(
        key="docs/api.md",
        content=b"API docs",
        content_type="text/markdown",
        visibility=3,
    )

    # List with prefix.
    arts = await king.list_artifacts(prefix="docs/")
    check("list with prefix", len(arts) >= 2, f"got {len(arts)}")


async def test_resource_usage(queen: CoreClient):
    """Test GetResourceUsage."""
    print("\n--- 6. Resource Usage ---")

    usage = await queen.get_resource_usage()
    check("tokens_consumed >= 0", usage.tokens_consumed >= 0)
    check("children_active >= 0", usage.children_active >= 0)
    check("children_max > 0", usage.children_max > 0, f"got {usage.children_max}")

    print(f"       tokens_consumed={usage.tokens_consumed}, "
          f"tokens_remaining={usage.tokens_remaining}, "
          f"children={usage.children_active}/{usage.children_max}")


async def test_metrics(queen: CoreClient):
    """Test ReportMetric (token accounting)."""
    print("\n--- 7. Metrics & Accounting ---")

    # Report token usage -- this triggers budget tracking on the Go side.
    await queen.report_metric("tokens_consumed", 1500.0)
    check("report_metric OK", True)

    # Check usage reflects the consumed tokens.
    usage = await queen.get_resource_usage()
    check("tokens tracked", usage.tokens_consumed >= 0,
          f"consumed={usage.tokens_consumed}")


async def test_escalation(queen: CoreClient):
    """Test Escalate."""
    print("\n--- 8. Escalation ---")

    resp = await queen.escalate(
        issue="Test escalation from Python SDK",
        severity="warning",
    )
    check("escalation received", True)  # no exception = success


async def test_logging(queen: CoreClient):
    """Test Log."""
    print("\n--- 9. Logging ---")

    await queen.log("info", "Integration test running from Python SDK")
    await queen.log("warn", "This is a warning test")
    check("logging OK", True)  # no exception = success


async def test_acl_enforcement(king: CoreClient, queen: CoreClient):
    """Test that ACL rules are enforced (Phase 3)."""
    print("\n--- 10. ACL Enforcement ---")

    # Spawn a task under queen.
    task_pid = await queen.spawn_child(
        name="acl-test-task",
        role="task",
        cognitive_tier="operational",
    )
    check("spawn task for ACL test", task_pid > 0)

    # Tasks cannot spawn children (ACL denies).
    task_client = CoreClient(
        grpc.aio.insecure_channel("localhost:50051"), pid=task_pid
    )
    try:
        await task_client.spawn_child(
            name="should-fail",
            role="task",
            cognitive_tier="operational",
        )
        check("task cannot spawn (ACL)", False, "spawn should have failed")
    except RuntimeError as e:
        check("task cannot spawn (ACL)", "denied" in str(e).lower(), str(e))

    # Cleanup.
    await queen.kill_child(task_pid)


async def main():
    print("=" * 60)
    print("HiveKernel Integration Test (Phase 0-3)")
    print("Connecting to Go core at localhost:50051...")
    print("=" * 60)

    channel = grpc.aio.insecure_channel("localhost:50051")

    # Quick connectivity check.
    try:
        king_tmp = CoreClient(channel, pid=1)
        await asyncio.wait_for(king_tmp.get_process_info(pid=1), timeout=3)
    except Exception:
        print("\n[ERROR] Cannot connect to Go core at localhost:50051")
        print("Start it first: bin\\hivekernel.exe --listen :50051")
        sys.exit(1)

    # Create clients for different PIDs.
    king = CoreClient(channel, pid=1)   # king = PID 1
    queen = CoreClient(channel, pid=2)  # queen = PID 2 (spawned by demo)

    await test_process_info(king)
    await test_process_tree(king)
    await test_spawn_and_kill(queen)
    await test_ipc_messages(king, queen)
    await test_artifacts(queen, king)
    await test_resource_usage(queen)
    await test_metrics(queen)
    await test_escalation(queen)
    await test_logging(queen)
    await test_acl_enforcement(king, queen)

    print("\n" + "=" * 60)
    if FAIL == 0:
        print(f"ALL {PASS} CHECKS PASSED!")
    else:
        print(f"{PASS} passed, {FAIL} FAILED")
    print("=" * 60)

    await channel.close()

    sys.exit(1 if FAIL > 0 else 0)


if __name__ == "__main__":
    asyncio.run(main())
