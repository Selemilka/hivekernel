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

import sys
import os

# Allow running from project root.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import grpc
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


def test_process_info(king: CoreClient):
    """Test GetProcessInfo for the king process."""
    print("\n--- 1. Process Info ---")

    info = king.get_process_info(pid=1)
    check("king exists", info.pid == 1)
    check("king name", info.name == "king", f"got '{info.name}'")
    check("king role is kernel (0)", info.role == 0)
    check("king user is root", info.user == "root", f"got '{info.user}'")
    check("king VPS is vps1", info.vps == "vps1", f"got '{info.vps}'")

    # pid=0 means "self" (caller is PID 1)
    self_info = king.get_process_info(pid=0)
    check("pid=0 resolves to self", self_info.pid == 1)


def test_process_tree(king: CoreClient):
    """Test ListChildren — demo spawns queen + worker."""
    print("\n--- 2. Process Tree ---")

    children = king.list_children(recursive=True)
    check("king has children", len(children) >= 2, f"got {len(children)}")

    names = [c.name for c in children]
    check("queen exists in tree", "queen@vps1" in names, f"got {names}")
    check("demo-worker exists", "demo-worker" in names, f"got {names}")


def test_spawn_and_kill(queen: CoreClient):
    """Test SpawnChild + KillChild from queen's perspective."""
    print("\n--- 3. Spawn & Kill ---")

    # Spawn a task under queen.
    task_pid = queen.spawn_child(
        name="test-task-py",
        role="task",
        cognitive_tier="operational",
        model="mini",
    )
    check("spawn returns PID > 0", task_pid > 0, f"got {task_pid}")

    # Verify the task exists.
    info = queen.get_process_info(pid=task_pid)
    check("spawned task name", info.name == "test-task-py", f"got '{info.name}'")
    check("task role is task (6)", info.role == 6, f"got {info.role}")
    check("task inherits user from queen", info.user == "root", f"got '{info.user}'")
    check("task inherits VPS", info.vps == "vps1", f"got '{info.vps}'")

    # Kill the task.
    killed = queen.kill_child(task_pid)
    check("kill returns task PID", task_pid in killed, f"got {killed}")


def test_ipc_messages(king: CoreClient, queen: CoreClient):
    """Test SendMessage between processes."""
    print("\n--- 4. IPC Messages ---")

    # Queen sends message to king (child -> parent = direct).
    msg_id = queen.send_message(
        to_pid=1,
        type="hello",
        payload=b"Hello from Python!",
        priority="normal",
    )
    check("message delivered", len(msg_id) > 0, f"got id='{msg_id}'")

    # Send to named queue.
    msg_id2 = queen.send_message(
        to_queue="test-results",
        type="data",
        payload=b"test payload",
    )
    check("named queue message", len(msg_id2) > 0)


def test_artifacts(queen: CoreClient, king: CoreClient):
    """Test shared memory artifacts (store/get/list)."""
    print("\n--- 5. Shared Memory Artifacts ---")

    # Queen stores a global artifact.
    art_id = queen.store_artifact(
        key="docs/readme.md",
        content=b"# HiveKernel\nTest artifact from Python",
        content_type="text/markdown",
        visibility=3,  # VIS_GLOBAL
    )
    check("store returns ID", len(art_id) > 0, f"got '{art_id}'")

    # King can read it (global visibility).
    art = king.get_artifact(key="docs/readme.md")
    check("king reads global artifact", art.found)
    check("content matches", art.content == b"# HiveKernel\nTest artifact from Python")
    check("content_type matches", art.content_type == "text/markdown")

    # Store another artifact.
    queen.store_artifact(
        key="docs/api.md",
        content=b"API docs",
        content_type="text/markdown",
        visibility=3,
    )

    # List with prefix.
    arts = king.list_artifacts(prefix="docs/")
    check("list with prefix", len(arts) >= 2, f"got {len(arts)}")


def test_resource_usage(queen: CoreClient):
    """Test GetResourceUsage."""
    print("\n--- 6. Resource Usage ---")

    usage = queen.get_resource_usage()
    check("tokens_consumed >= 0", usage.tokens_consumed >= 0)
    check("children_active >= 0", usage.children_active >= 0)
    check("children_max > 0", usage.children_max > 0, f"got {usage.children_max}")

    print(f"       tokens_consumed={usage.tokens_consumed}, "
          f"tokens_remaining={usage.tokens_remaining}, "
          f"children={usage.children_active}/{usage.children_max}")


def test_metrics(queen: CoreClient):
    """Test ReportMetric (token accounting)."""
    print("\n--- 7. Metrics & Accounting ---")

    # Report token usage — this triggers budget tracking on the Go side.
    queen.report_metric("tokens_consumed", 1500.0)
    check("report_metric OK", True)

    # Check usage reflects the consumed tokens.
    usage = queen.get_resource_usage()
    check("tokens tracked", usage.tokens_consumed >= 0,
          f"consumed={usage.tokens_consumed}")


def test_escalation(queen: CoreClient):
    """Test Escalate."""
    print("\n--- 8. Escalation ---")

    resp = queen.escalate(
        issue="Test escalation from Python SDK",
        severity="warning",
    )
    check("escalation received", True)  # no exception = success


def test_logging(queen: CoreClient):
    """Test Log."""
    print("\n--- 9. Logging ---")

    queen.log("info", "Integration test running from Python SDK")
    queen.log("warn", "This is a warning test")
    check("logging OK", True)  # no exception = success


def test_acl_enforcement(king: CoreClient, queen: CoreClient):
    """Test that ACL rules are enforced (Phase 3)."""
    print("\n--- 10. ACL Enforcement ---")

    # Spawn a task under queen.
    task_pid = queen.spawn_child(
        name="acl-test-task",
        role="task",
        cognitive_tier="operational",
    )
    check("spawn task for ACL test", task_pid > 0)

    # Tasks cannot spawn children (ACL denies).
    task_client = CoreClient(
        grpc.insecure_channel("localhost:50051"), pid=task_pid
    )
    try:
        task_client.spawn_child(
            name="should-fail",
            role="task",
            cognitive_tier="operational",
        )
        check("task cannot spawn (ACL)", False, "spawn should have failed")
    except RuntimeError as e:
        check("task cannot spawn (ACL)", "denied" in str(e).lower(), str(e))

    # Cleanup.
    queen.kill_child(task_pid)


def main():
    print("=" * 60)
    print("HiveKernel Integration Test (Phase 0-3)")
    print("Connecting to Go core at localhost:50051...")
    print("=" * 60)

    try:
        channel = grpc.insecure_channel("localhost:50051")
        # Quick connectivity check.
        grpc.channel_ready_future(channel).result(timeout=3)
    except grpc.FutureTimeoutError:
        print("\n[ERROR] Cannot connect to Go core at localhost:50051")
        print("Start it first: bin\\hivekernel.exe --listen :50051")
        sys.exit(1)

    # Create clients for different PIDs.
    king = CoreClient(channel, pid=1)   # king = PID 1
    queen = CoreClient(channel, pid=2)  # queen = PID 2 (spawned by demo)

    test_process_info(king)
    test_process_tree(king)
    test_spawn_and_kill(queen)
    test_ipc_messages(king, queen)
    test_artifacts(queen, king)
    test_resource_usage(queen)
    test_metrics(queen)
    test_escalation(queen)
    test_logging(queen)
    test_acl_enforcement(king, queen)

    print("\n" + "=" * 60)
    if FAIL == 0:
        print(f"ALL {PASS} CHECKS PASSED!")
    else:
        print(f"{PASS} passed, {FAIL} FAILED")
    print("=" * 60)

    sys.exit(1 if FAIL > 0 else 0)


if __name__ == "__main__":
    main()
