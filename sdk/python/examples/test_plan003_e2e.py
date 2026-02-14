"""
HiveKernel E2E Test: Plan 003 -- Practical Agents
==================================================

Verifies all 4 new agent phases:
  Phase 1: Cron Engine (AddCron / ListCron / RemoveCron gRPC)
  Phase 2: Assistant Agent (chat via execute_on, scheduling, delegation)
  Phase 3: GitHub Monitor (cron-triggered, commit checking)
  Phase 4: Coder Agent (code generation via execute_on)

Requires:
    - .env file with OPENROUTER_API_KEY=sk-or-v1-...
    - Built kernel: go build -o bin/hivekernel.exe ./cmd/hivekernel

Usage:
    python sdk/python/examples/test_plan003_e2e.py                # with dashboard
    python sdk/python/examples/test_plan003_e2e.py --no-dashboard # without dashboard
    python sdk/python/examples/test_plan003_e2e.py --no-llm       # skip LLM tests
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

import grpc  # noqa: E402
import grpc.aio  # noqa: E402
from hivekernel_sdk import agent_pb2, core_pb2, core_pb2_grpc  # noqa: E402

KERNEL_BIN = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "..", "..", "bin", "hivekernel.exe")
)
DASHBOARD_APP = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "dashboard", "app.py")
)
CORE_ADDR = "localhost:50056"
CORE_LISTEN = ":50056"
DASHBOARD_PORT = 8081

KING_MD = [("x-hivekernel-pid", "1")]
ROLES = {0: "kernel", 1: "daemon", 2: "agent", 3: "architect",
         4: "lead", 5: "worker", 6: "task"}
TIERS = {0: "strategic", 1: "tactical", 2: "operational"}
STATES = {0: "idle", 1: "running", 2: "blocked", 3: "sleeping",
          4: "dead", 5: "zombie"}


def load_dotenv():
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


async def wait_for_kernel(addr, timeout=15.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            channel = grpc.aio.insecure_channel(addr)
            stub = core_pb2_grpc.CoreServiceStub(channel)
            await stub.GetProcessInfo(
                core_pb2.ProcessInfoRequest(pid=1),
                metadata=KING_MD, timeout=2,
            )
            await channel.close()
            return
        except Exception:
            await asyncio.sleep(0.3)
    raise TimeoutError(f"Kernel at {addr} not ready after {timeout}s")


async def print_tree(stub):
    king = await stub.GetProcessInfo(
        core_pb2.ProcessInfoRequest(pid=1), metadata=KING_MD)
    children = await stub.ListChildren(
        core_pb2.ListChildrenRequest(recursive=True), metadata=KING_MD)
    print("  PID  PPID  ROLE        TIER          STATE     NAME")
    print("  ---  ----  ----------  ------------  --------  ----")
    all_procs = [king] + list(children.children)
    for p in all_procs:
        print(f"  {p.pid:<4} {p.ppid:<5} {ROLES.get(p.role, '?'):<10}  "
              f"{TIERS.get(p.cognitive_tier, '?'):<12}  "
              f"{STATES.get(p.state, '?'):<8}  {p.name}")
    return all_procs


def find_pid_by_name(procs, name):
    for p in procs:
        if p.name == name:
            return p.pid
    # Fallback: prefix match (queen@local matches queen@vps1).
    for p in procs:
        if p.name.startswith(name.split("@")[0] + "@"):
            return p.pid
    return None


async def async_pause(msg="Press Enter to continue..."):
    await asyncio.to_thread(input, f"\n  >>> {msg}")


# ============================================================
# Phase Tests
# ============================================================

async def test_phase1_cron_engine(stub, queen_pid):
    """Phase 1: Cron Engine -- AddCron, ListCron, RemoveCron."""
    print("\n" + "=" * 60)
    print("  PHASE 1: Cron Engine Activation")
    print("=" * 60)

    # 1. ListCron -- should include default github-check from Queen
    list_resp = await stub.ListCron(
        core_pb2.ListCronRequest(), metadata=KING_MD)
    entries = list(list_resp.entries)
    print(f"  Initial cron entries: {len(entries)}")
    for e in entries:
        print(f"    [{e.id}] {e.name}: cron={e.cron_expression}, "
              f"target_pid={e.target_pid}, desc={e.execute_description[:50]}")

    github_cron = [e for e in entries if e.name == "github-check"]
    has_default = len(github_cron) > 0
    print(f"  Default github-check cron exists: {has_default}")

    # 2. AddCron -- add a test cron entry
    add_resp = await stub.AddCron(
        core_pb2.AddCronRequest(
            name="test-cron-003",
            cron_expression="0 */6 * * *",
            target_pid=queen_pid,
            execute_description="Test cron from Plan 003 E2E",
            execute_params={"test": "true"},
        ), metadata=KING_MD)
    cron_id = add_resp.cron_id
    print(f"  Added test cron: id={cron_id}")

    # 3. ListCron -- verify new entry
    list_resp2 = await stub.ListCron(
        core_pb2.ListCronRequest(), metadata=KING_MD)
    entries2 = list(list_resp2.entries)
    test_found = any(e.name == "test-cron-003" for e in entries2)
    print(f"  Test cron found in list: {test_found}")

    # 4. RemoveCron -- remove test entry
    await stub.RemoveCron(
        core_pb2.RemoveCronRequest(cron_id=cron_id), metadata=KING_MD)
    print(f"  Removed test cron: id={cron_id}")

    list_resp3 = await stub.ListCron(
        core_pb2.ListCronRequest(), metadata=KING_MD)
    entries3 = list(list_resp3.entries)
    test_gone = not any(e.name == "test-cron-003" for e in entries3)
    print(f"  Test cron removed from list: {test_gone}")

    ok = has_default and test_found and test_gone
    print(f"\n  [{'PASS' if ok else 'FAIL'}] Cron Engine")
    return ok


async def test_phase2_assistant(stub, procs, no_llm=False):
    """Phase 2: Assistant Agent -- spawned, chat works."""
    print("\n" + "=" * 60)
    print("  PHASE 2: Assistant Agent")
    print("=" * 60)

    assistant_pid = find_pid_by_name(procs, "assistant")
    if not assistant_pid:
        print("  [FAIL] Assistant not found in process tree")
        return False

    info = await stub.GetProcessInfo(
        core_pb2.ProcessInfoRequest(pid=assistant_pid), metadata=KING_MD)
    print(f"  Assistant: PID {assistant_pid}, role={ROLES.get(info.role)}, "
          f"tier={TIERS.get(info.cognitive_tier)}, state={STATES.get(info.state)}")

    alive = info.state in (0, 1)
    is_daemon = info.role == agent_pb2.ROLE_DAEMON
    print(f"  Is daemon: {is_daemon}, Is alive: {alive}")

    if no_llm:
        print("  [SKIP] Chat test requires LLM (--no-llm)")
        return is_daemon and alive

    # Chat test: send a message via execute_on
    print("  Sending chat message: 'Hello, what can you do?'...")
    try:
        exec_resp = await stub.ExecuteTask(
            core_pb2.ExecuteTaskRequest(
                target_pid=assistant_pid,
                description="Chat message",
                params={
                    "message": "Hello, what can you do?",
                    "history": "[]",
                    "sibling_pids": "{}",
                },
                timeout_seconds=60,
            ), metadata=KING_MD, timeout=120)

        print(f"  Response exit_code: {exec_resp.result.exit_code}")
        output = exec_resp.result.output[:200]
        print(f"  Response: {output}")
        chat_ok = exec_resp.result.exit_code == 0 and len(exec_resp.result.output) > 10
        print(f"  [{'PASS' if chat_ok else 'FAIL'}] Chat response")
    except Exception as e:
        print(f"  [FAIL] Chat error: {e}")
        chat_ok = False

    ok = is_daemon and alive and chat_ok
    print(f"\n  [{'PASS' if ok else 'FAIL'}] Assistant Agent")
    return ok


async def test_phase3_github_monitor(stub, procs):
    """Phase 3: GitHub Monitor -- spawned, cron scheduled."""
    print("\n" + "=" * 60)
    print("  PHASE 3: GitHub Monitor")
    print("=" * 60)

    gm_pid = find_pid_by_name(procs, "github-monitor")
    if not gm_pid:
        print("  [FAIL] GitHub Monitor not found in process tree")
        return False

    info = await stub.GetProcessInfo(
        core_pb2.ProcessInfoRequest(pid=gm_pid), metadata=KING_MD)
    print(f"  GitMonitor: PID {gm_pid}, role={ROLES.get(info.role)}, "
          f"tier={TIERS.get(info.cognitive_tier)}, state={STATES.get(info.state)}")

    alive = info.state in (0, 1)
    is_daemon = info.role == agent_pb2.ROLE_DAEMON
    is_operational = info.cognitive_tier == agent_pb2.COG_OPERATIONAL
    print(f"  Is daemon: {is_daemon}, Is alive: {alive}, "
          f"Is operational: {is_operational}")

    # Check that cron entry targets this PID
    list_resp = await stub.ListCron(
        core_pb2.ListCronRequest(), metadata=KING_MD)
    gh_crons = [e for e in list_resp.entries
                if e.target_pid == gm_pid]
    print(f"  Cron entries targeting GitMonitor: {len(gh_crons)}")
    for c in gh_crons:
        print(f"    [{c.id}] {c.name}: {c.cron_expression}")

    has_cron = len(gh_crons) > 0
    ok = alive and is_daemon and is_operational and has_cron
    print(f"\n  [{'PASS' if ok else 'FAIL'}] GitHub Monitor")
    return ok


async def test_phase4_coder(stub, procs, no_llm=False):
    """Phase 4: Coder Agent -- spawned, code generation works."""
    print("\n" + "=" * 60)
    print("  PHASE 4: Coder Agent")
    print("=" * 60)

    coder_pid = find_pid_by_name(procs, "coder")
    if not coder_pid:
        print("  [FAIL] Coder not found in process tree")
        return False

    info = await stub.GetProcessInfo(
        core_pb2.ProcessInfoRequest(pid=coder_pid), metadata=KING_MD)
    print(f"  Coder: PID {coder_pid}, role={ROLES.get(info.role)}, "
          f"tier={TIERS.get(info.cognitive_tier)}, state={STATES.get(info.state)}")

    alive = info.state in (0, 1)
    is_daemon = info.role == agent_pb2.ROLE_DAEMON
    is_tactical = info.cognitive_tier == agent_pb2.COG_TACTICAL
    print(f"  Is daemon: {is_daemon}, Is alive: {alive}, "
          f"Is tactical: {is_tactical}")

    if no_llm:
        print("  [SKIP] Code generation test requires LLM (--no-llm)")
        return is_daemon and alive

    # Code generation test
    print("  Requesting: 'Write a Python function for Fibonacci numbers'...")
    try:
        exec_resp = await stub.ExecuteTask(
            core_pb2.ExecuteTaskRequest(
                target_pid=coder_pid,
                description="Write Fibonacci function",
                params={
                    "request": "Write a Python function that returns the n-th Fibonacci number",
                    "language": "python",
                },
                timeout_seconds=60,
            ), metadata=KING_MD, timeout=120)

        print(f"  Response exit_code: {exec_resp.result.exit_code}")
        code = exec_resp.result.output
        preview = code[:300] if code else "(empty)"
        print(f"  Generated code:\n    {preview.replace(chr(10), chr(10) + '    ')}")
        has_def = "def " in code or "fibonacci" in code.lower()
        code_ok = exec_resp.result.exit_code == 0 and has_def
        print(f"  Contains function definition: {has_def}")
        print(f"  [{'PASS' if code_ok else 'FAIL'}] Code generation")
    except Exception as e:
        print(f"  [FAIL] Code generation error: {e}")
        code_ok = False

    ok = is_daemon and alive and code_ok
    print(f"\n  [{'PASS' if ok else 'FAIL'}] Coder Agent")
    return ok


async def test_full_tree(stub, expected_names):
    """Verify all expected agents are in the tree."""
    print("\n" + "=" * 60)
    print("  FULL TREE: All Agents Spawned")
    print("=" * 60)

    procs = await print_tree(stub)

    proc_names = {p.name for p in procs}
    results = {}
    for name in expected_names:
        found = name in proc_names
        results[name] = found
        status = "OK" if found else "MISSING"
        print(f"  {name}: {status}")

    ok = all(results.values())
    print(f"\n  [{'PASS' if ok else 'FAIL'}] Full tree check")
    return ok, procs


# ============================================================
# Main
# ============================================================

async def run_all_tests(with_dashboard, no_llm):
    channel = grpc.aio.insecure_channel(CORE_ADDR)
    stub = core_pb2_grpc.CoreServiceStub(channel)

    try:
        # Wait for all agents to spawn (Queen + 4 children)
        print("\n--- Waiting for agents to spawn (10s)... ---")
        await asyncio.sleep(10)

        expected = ["king", "maid@local",
                    "assistant", "github-monitor", "coder"]

        # Full tree check
        tree_ok, procs = await test_full_tree(stub, expected)

        queen_pid = find_pid_by_name(procs, "queen@local")
        if not queen_pid:
            print("[FATAL] Queen not found!")
            return False

        if with_dashboard:
            await async_pause(
                "Check dashboard in browser. Press Enter to start tests...")

        results = {}

        # Phase 1: Cron Engine
        results["phase1_cron"] = await test_phase1_cron_engine(stub, queen_pid)

        # Phase 2: Assistant
        results["phase2_assistant"] = await test_phase2_assistant(
            stub, procs, no_llm)

        if with_dashboard:
            await async_pause(
                "Phase 1-2 done. Press Enter for Phase 3-4...")

        # Phase 3: GitHub Monitor
        results["phase3_github"] = await test_phase3_github_monitor(stub, procs)

        # Phase 4: Coder
        results["phase4_coder"] = await test_phase4_coder(
            stub, procs, no_llm)

        # Final tree
        print("\n--- Final Process Tree ---")
        await print_tree(stub)

        # Summary
        print("\n" + "=" * 60)
        print("  SUMMARY: Plan 003 Verification")
        print("=" * 60)
        print(f"  Full tree:       {'PASS' if tree_ok else 'FAIL'}")
        all_ok = tree_ok
        for phase, ok in results.items():
            status = "PASS" if ok else "FAIL"
            if not ok:
                all_ok = False
            print(f"  {phase:20s} {status}")

        if all_ok:
            print("\n  ALL PHASES PASSED!")
        else:
            print("\n  Some phases had issues (check WARN/FAIL above)")

        if with_dashboard:
            await async_pause("All done! Press Enter to shut down...")

        return all_ok

    finally:
        await channel.close()


def main():
    print("=" * 60)
    print("  HiveKernel E2E: Plan 003 -- Practical Agents")
    print("=" * 60)

    with_dashboard = "--no-dashboard" not in sys.argv
    no_llm = "--no-llm" in sys.argv

    load_dotenv()

    api_key = os.environ.get("OPENROUTER_API_KEY", "")
    if not api_key and not no_llm:
        print("\nWARN: No OPENROUTER_API_KEY. LLM tests will be skipped.")
        no_llm = True
    elif api_key:
        print(f"  API key: {api_key[:12]}...{api_key[-4:]}")

    if not os.path.isfile(KERNEL_BIN):
        print(f"\nFATAL: Kernel not found: {KERNEL_BIN}")
        print("Build: go build -o bin/hivekernel.exe ./cmd/hivekernel")
        sys.exit(1)

    env = os.environ.copy()
    pythonpath = env.get("PYTHONPATH", "")
    if SDK_DIR not in pythonpath:
        env["PYTHONPATH"] = (SDK_DIR + os.pathsep + pythonpath
                             if pythonpath else SDK_DIR)

    # Start kernel
    print("\n--- Starting Kernel ---")
    kernel_log = os.path.join(os.path.dirname(KERNEL_BIN), "kernel_e2e.log")
    kernel_log_f = open(kernel_log, "w")
    kernel_proc = subprocess.Popen(
        [KERNEL_BIN, "--listen", CORE_LISTEN],
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=kernel_log_f,
    )
    print(f"  Kernel log: {kernel_log}")
    print(f"  Kernel started (OS PID {kernel_proc.pid})")

    # Start dashboard
    dashboard_proc = None
    if with_dashboard:
        if os.path.isfile(DASHBOARD_APP):
            try:
                dash_env = env.copy()
                dash_env["HIVEKERNEL_ADDR"] = CORE_ADDR
                dash_env["DASHBOARD_PORT"] = str(DASHBOARD_PORT)
                dashboard_proc = subprocess.Popen(
                    [sys.executable, DASHBOARD_APP],
                    env=dash_env,
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                )
                print(f"  Dashboard started on port {DASHBOARD_PORT}")
            except Exception as e:
                print(f"  Dashboard failed to start: {e}")

    try:
        # Wait for kernel
        print("  Waiting for kernel...")
        asyncio.run(wait_for_kernel(CORE_ADDR, timeout=20.0))
        print("  Kernel ready!")

        if with_dashboard and dashboard_proc:
            time.sleep(1)
            webbrowser.open(f"http://localhost:{DASHBOARD_PORT}")

        success = asyncio.run(run_all_tests(with_dashboard, no_llm))
        sys.exit(0 if success else 1)

    except TimeoutError as e:
        print(f"\n  FATAL: {e}")
        print(f"  Check kernel log: {kernel_log}")
        sys.exit(1)

    finally:
        if dashboard_proc:
            dashboard_proc.terminate()
            print("  Dashboard stopped")
        kernel_proc.terminate()
        kernel_log_f.close()
        print("  Kernel stopped")


if __name__ == "__main__":
    main()
