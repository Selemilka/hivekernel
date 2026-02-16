"""
HiveKernel Live Demo Client
============================

Connects to a running kernel via gRPC and sends tasks to agents.
Results visible in console and in the web dashboard.

Prerequisites:
    # Terminal 1: start kernel with agents
    set OPENROUTER_API_KEY=sk-or-v1-...
    bin\\hivekernel.exe --listen :50051 --startup configs/startup-full.json

    # Terminal 2 (optional): start dashboard for visual
    python sdk/python/dashboard/app.py

    # Terminal 3: run demo
    python sdk/python/examples/demo_live.py showcase

Commands:
    maid       Health check (no LLM, ~3s)
    github     GitHub repo check (no LLM, ~5s)
    assistant  Ask a question (LLM, ~15s)
    coder      Generate code (LLM, ~20s)
    queen      Full pipeline: lead + workers (LLM, ~60-120s)  -- STAR DEMO
    showcase   Run all demos sequentially with pauses

Options:
    --addr     Kernel gRPC address (default: localhost:50051)

Examples:
    python sdk/python/examples/demo_live.py maid
    python sdk/python/examples/demo_live.py queen
    python sdk/python/examples/demo_live.py queen "Design a REST API for a todo app"
    python sdk/python/examples/demo_live.py showcase --addr localhost:50055
"""

import argparse
import asyncio
import json
import os
import sys
import time

SDK_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, SDK_DIR)

import grpc.aio  # noqa: E402
from hivekernel_sdk import core_pb2, core_pb2_grpc  # noqa: E402

# --- Constants ---

KING_MD = [("x-hivekernel-pid", "1")]

ROLES = {0: "kernel", 1: "daemon", 2: "agent", 3: "architect",
         4: "lead", 5: "worker", 6: "task"}
TIERS = {0: "strategic", 1: "tactical", 2: "operational"}
STATES = {0: "idle", 1: "running", 2: "blocked", 3: "sleeping",
          4: "dead", 5: "zombie"}

# Default tasks for each demo
DEFAULT_QUEEN_TASK = (
    "Research and analyze how multi-agent LLM systems compare "
    "to single-agent approaches for complex software development tasks"
)
DEFAULT_ASSISTANT_QUESTION = (
    "What is HiveKernel and how does its process tree model work?"
)
DEFAULT_CODER_REQUEST = (
    "Write a Python async context manager for rate limiting API calls"
)

# --- Helpers ---


def print_header(title):
    """Print a section header."""
    width = max(len(title) + 4, 50)
    line = "=" * width
    print(f"\n{line}")
    print(f"  {title}")
    print(line)


def print_step(n, text):
    """Print a numbered step."""
    print(f"  [{n}] {text}")


async def wait_for_kernel(addr, timeout=10.0):
    """Probe kernel until it responds or timeout."""
    deadline = time.time() + timeout
    last_err = None
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
        except Exception as e:
            last_err = e
            await asyncio.sleep(0.3)
    raise ConnectionError(
        f"Cannot reach kernel at {addr} after {timeout}s "
        f"(last error: {last_err})\n\n"
        f"Make sure the kernel is running:\n"
        f"  bin\\hivekernel.exe --listen :50051 "
        f"--startup configs/startup-full.json"
    )


async def find_agent_pid(stub, name_prefix):
    """Find agent PID by name prefix. Raises RuntimeError if not found."""
    king = await stub.GetProcessInfo(
        core_pb2.ProcessInfoRequest(pid=1), metadata=KING_MD)
    children = await stub.ListChildren(
        core_pb2.ListChildrenRequest(recursive=True), metadata=KING_MD)
    all_procs = [king] + list(children.children)

    # Exact match first
    for p in all_procs:
        if p.name == name_prefix:
            return p.pid

    # Prefix match (e.g. "queen" matches "queen@vps1")
    for p in all_procs:
        base = p.name.split("@")[0]
        if base == name_prefix:
            return p.pid

    # Substring match
    for p in all_procs:
        if name_prefix in p.name:
            return p.pid

    available = [p.name for p in all_procs if p.state in (0, 1)]
    raise RuntimeError(
        f"Agent '{name_prefix}' not found.\n"
        f"  Available agents: {', '.join(available)}"
    )


async def print_process_table(stub):
    """Print formatted process table."""
    king = await stub.GetProcessInfo(
        core_pb2.ProcessInfoRequest(pid=1), metadata=KING_MD)
    children = await stub.ListChildren(
        core_pb2.ListChildrenRequest(recursive=True), metadata=KING_MD)
    all_procs = [king] + list(children.children)

    print("  PID  PPID  ROLE        TIER          STATE     NAME")
    print("  ---  ----  ----------  ------------  --------  ----")
    for p in all_procs:
        print(f"  {p.pid:<4} {p.ppid:<5} {ROLES.get(p.role, '?'):<10}  "
              f"{TIERS.get(p.cognitive_tier, '?'):<12}  "
              f"{STATES.get(p.state, '?'):<8}  {p.name}")
    return all_procs


async def build_sibling_map(stub):
    """Build {name: pid} map of all king's children (siblings)."""
    children = await stub.ListChildren(
        core_pb2.ListChildrenRequest(recursive=False), metadata=KING_MD)
    result = {}
    for p in children.children:
        base = p.name.split("@")[0]
        result[base] = p.pid
        result[p.name] = p.pid
    return result


async def async_pause(msg="Press Enter to continue..."):
    """Non-blocking pause for user input."""
    await asyncio.to_thread(input, f"\n  >>> {msg}")


# --- Demo Functions ---


async def demo_maid(stub):
    """Health check demo -- no LLM, fast."""
    print_header("DEMO: Maid Health Check")
    print("  The Maid daemon scans the process table for zombies,")
    print("  orphans, and other anomalies. No LLM needed.\n")

    print_step(1, "Finding maid agent...")
    pid = await find_agent_pid(stub, "maid")
    print(f"      Maid PID: {pid}")

    print_step(2, "Sending health check request...")
    t0 = time.time()
    resp = await stub.ExecuteTask(
        core_pb2.ExecuteTaskRequest(
            target_pid=pid,
            description="Run health check",
            timeout_seconds=30,
        ), metadata=KING_MD, timeout=35)
    elapsed = time.time() - t0

    if not resp.success:
        print(f"  ERROR: {resp.error}")
        return False

    print_step(3, f"Health report received ({elapsed:.1f}s):")
    print()
    for line in resp.result.output.split("\n"):
        print(f"    {line}")
    print()
    print(f"  Result: exit_code={resp.result.exit_code}")
    return resp.result.exit_code == 0


async def demo_github(stub):
    """GitHub repo check demo -- no LLM, uses GitHub API."""
    print_header("DEMO: GitHub Monitor")
    print("  The GitHub Monitor checks repos for new commits")
    print("  using the GitHub API (unauthenticated).\n")

    print_step(1, "Finding github-monitor agent...")
    pid = await find_agent_pid(stub, "github-monitor")
    print(f"      GitHub Monitor PID: {pid}")

    print_step(2, "Checking Selemilka/hivekernel for updates...")
    t0 = time.time()
    resp = await stub.ExecuteTask(
        core_pb2.ExecuteTaskRequest(
            target_pid=pid,
            description="Check GitHub repos",
            params={"repos": '["Selemilka/hivekernel"]'},
            timeout_seconds=30,
        ), metadata=KING_MD, timeout=35)
    elapsed = time.time() - t0

    if not resp.success:
        print(f"  ERROR: {resp.error}")
        return False

    print_step(3, f"Result ({elapsed:.1f}s):")
    print()
    for line in resp.result.output.split("\n"):
        print(f"    {line}")
    print()
    print(f"  Result: exit_code={resp.result.exit_code}")
    return resp.result.exit_code == 0


async def demo_assistant(stub, question=None):
    """Chat assistant demo -- requires LLM."""
    print_header("DEMO: Assistant Chat")
    msg = question or DEFAULT_ASSISTANT_QUESTION
    print(f"  Sending question to the Assistant daemon (LLM-powered).")
    print(f"  Question: \"{msg}\"\n")

    print_step(1, "Finding assistant agent...")
    pid = await find_agent_pid(stub, "assistant")
    print(f"      Assistant PID: {pid}")

    print_step(2, "Building sibling map for delegation...")
    siblings = await build_sibling_map(stub)
    sibling_json = json.dumps({k: v for k, v in siblings.items()
                               if not k.startswith("king")})
    print(f"      Siblings: {sibling_json}")

    print_step(3, "Sending question...")
    t0 = time.time()
    resp = await stub.ExecuteTask(
        core_pb2.ExecuteTaskRequest(
            target_pid=pid,
            description="Chat message",
            params={
                "message": msg,
                "history": "[]",
                "sibling_pids": sibling_json,
            },
            timeout_seconds=60,
        ), metadata=KING_MD, timeout=65)
    elapsed = time.time() - t0

    if not resp.success:
        print(f"  ERROR: {resp.error}")
        return False

    print_step(4, f"Response ({elapsed:.1f}s):")
    print()
    output = resp.result.output
    if len(output) > 2000:
        output = output[:2000] + "\n... (truncated)"
    for line in output.split("\n"):
        print(f"    {line}")
    print()
    print(f"  Result: exit_code={resp.result.exit_code}")
    return resp.result.exit_code == 0


async def demo_coder(stub, request=None):
    """Code generation demo -- requires LLM."""
    print_header("DEMO: Coder Agent")
    req = request or DEFAULT_CODER_REQUEST
    print(f"  Sending a code generation request to the Coder daemon.")
    print(f"  Request: \"{req}\"\n")

    print_step(1, "Finding coder agent...")
    pid = await find_agent_pid(stub, "coder")
    print(f"      Coder PID: {pid}")

    print_step(2, "Requesting code generation...")
    t0 = time.time()
    resp = await stub.ExecuteTask(
        core_pb2.ExecuteTaskRequest(
            target_pid=pid,
            description="Generate code",
            params={
                "request": req,
                "language": "python",
            },
            timeout_seconds=60,
        ), metadata=KING_MD, timeout=65)
    elapsed = time.time() - t0

    if not resp.success:
        print(f"  ERROR: {resp.error}")
        return False

    print_step(3, f"Generated code ({elapsed:.1f}s):")
    print()
    code = resp.result.output
    if len(code) > 3000:
        code = code[:3000] + "\n... (truncated)"
    for line in code.split("\n"):
        print(f"    {line}")
    print()
    print(f"  Result: exit_code={resp.result.exit_code}")
    return resp.result.exit_code == 0


async def demo_queen(stub, task=None):
    """Full pipeline demo -- the star of the show.

    Queen receives the task, assesses complexity, spawns lead + workers.
    Watch the dashboard to see nodes appear, work, and die.
    """
    print_header("DEMO: Queen Full Pipeline  ** STAR DEMO **")
    desc = task or DEFAULT_QUEEN_TASK
    print("  This is the most visually impressive demo. If you have the")
    print("  dashboard open, watch the tree as nodes spawn and die.\n")
    print("  Pipeline:")
    print("    1. Queen receives task, assesses complexity")
    print("    2. Queen spawns/reuses a Lead (orchestrator)")
    print("    3. Lead decomposes task, spawns Workers")
    print("    4. Workers research in parallel (all running)")
    print("    5. Workers complete (become zombies, gray in dashboard)")
    print("    6. Lead synthesizes results")
    print("    7. Zombies get reaped (nodes disappear)")
    print()
    print(f"  Task: \"{desc}\"")
    print()

    print_step(1, "Finding queen agent...")
    pid = await find_agent_pid(stub, "queen")
    print(f"      Queen PID: {pid}")

    print_step(2, "Process table BEFORE:")
    print()
    await print_process_table(stub)
    print()

    print_step(3, "Sending task to Queen (this may take 1-3 minutes)...")
    t0 = time.time()
    try:
        resp = await stub.ExecuteTask(
            core_pb2.ExecuteTaskRequest(
                target_pid=pid,
                description=desc,
                params={
                    "task": desc,
                    "max_workers": "3",
                },
                timeout_seconds=300,
            ), metadata=KING_MD, timeout=305)
    except grpc.aio.AioRpcError as e:
        elapsed = time.time() - t0
        print(f"  gRPC error after {elapsed:.1f}s: {e.code()} - {e.details()}")
        return False
    elapsed = time.time() - t0

    if not resp.success:
        print(f"  ERROR after {elapsed:.1f}s: {resp.error}")
        return False

    print_step(4, f"Task completed ({elapsed:.1f}s)")

    # Print metadata (strategy, tokens, etc.)
    meta = dict(resp.result.metadata) if resp.result.metadata else {}
    if meta:
        print()
        print("  Metadata:")
        for k, v in meta.items():
            print(f"    {k}: {v}")

    # Print result (truncated)
    print()
    print_step(5, "Result:")
    print()
    output = resp.result.output
    if len(output) > 1500:
        output = output[:1500] + "\n\n... (truncated, full output: %d chars)" % len(resp.result.output)
    for line in output.split("\n"):
        print(f"    {line}")

    # Print process table after
    print()
    print_step(6, "Process table AFTER:")
    print()
    await print_process_table(stub)

    print()
    print(f"  Result: exit_code={resp.result.exit_code}, "
          f"time={elapsed:.1f}s")
    return resp.result.exit_code == 0


async def demo_showcase(stub):
    """Run all demos sequentially with pauses."""
    print_header("HIVEKERNEL SHOWCASE")
    print("  Running all 5 demos sequentially.")
    print("  Press Enter between demos to continue.")
    print("  Open the dashboard to watch the tree in real-time!")

    results = {}

    # 1. Maid (always works)
    try:
        results["maid"] = await demo_maid(stub)
    except Exception as e:
        print(f"  FAILED: {e}")
        results["maid"] = False
    await async_pause("Maid demo done. Press Enter for GitHub Monitor...")

    # 2. GitHub
    try:
        results["github"] = await demo_github(stub)
    except Exception as e:
        print(f"  FAILED: {e}")
        results["github"] = False
    await async_pause("GitHub demo done. Press Enter for Assistant...")

    # 3. Assistant
    try:
        results["assistant"] = await demo_assistant(stub)
    except Exception as e:
        print(f"  FAILED: {e}")
        results["assistant"] = False
    await async_pause("Assistant demo done. Press Enter for Coder...")

    # 4. Coder
    try:
        results["coder"] = await demo_coder(stub)
    except Exception as e:
        print(f"  FAILED: {e}")
        results["coder"] = False
    await async_pause("Coder demo done. Press Enter for Queen (STAR DEMO)...")

    # 5. Queen
    try:
        results["queen"] = await demo_queen(stub)
    except Exception as e:
        print(f"  FAILED: {e}")
        results["queen"] = False

    # Summary
    print_header("SHOWCASE RESULTS")
    print()
    print("  Demo           Result")
    print("  -------------  ------")
    for name, ok in results.items():
        status = "[OK]  " if ok else "[FAIL]"
        print(f"  {name:<13}  {status}")

    total = sum(1 for v in results.values() if v)
    print()
    print(f"  {total}/{len(results)} demos passed.")
    return all(results.values())


# --- Entry Point ---


async def main_async(args):
    """Connect to kernel and dispatch command."""
    addr = args.addr
    cmd = args.command

    print(f"  Connecting to kernel at {addr}...")
    try:
        await wait_for_kernel(addr, timeout=10.0)
    except ConnectionError as e:
        print(f"\n  ERROR: {e}")
        return False
    print(f"  Kernel is ready.\n")

    channel = grpc.aio.insecure_channel(addr)
    stub = core_pb2_grpc.CoreServiceStub(channel)

    try:
        if cmd == "maid":
            return await demo_maid(stub)
        elif cmd == "github":
            return await demo_github(stub)
        elif cmd == "assistant":
            return await demo_assistant(stub, args.task_text)
        elif cmd == "coder":
            return await demo_coder(stub, args.task_text)
        elif cmd == "queen":
            return await demo_queen(stub, args.task_text)
        elif cmd == "showcase":
            return await demo_showcase(stub)
        else:
            print(f"  Unknown command: {cmd}")
            print("  Available: maid, github, assistant, coder, queen, showcase")
            return False
    except RuntimeError as e:
        print(f"\n  ERROR: {e}")
        return False
    except grpc.aio.AioRpcError as e:
        print(f"\n  gRPC ERROR: {e.code()} - {e.details()}")
        return False
    finally:
        await channel.close()


def main():
    parser = argparse.ArgumentParser(
        description="HiveKernel Live Demo Client",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "examples:\n"
            "  %(prog)s maid                        # health check (no LLM)\n"
            "  %(prog)s github                      # repo check (no LLM)\n"
            "  %(prog)s assistant                    # chat (LLM)\n"
            "  %(prog)s coder                        # code gen (LLM)\n"
            "  %(prog)s queen                        # full pipeline (LLM)\n"
            '  %(prog)s queen "Design a REST API"    # custom task\n'
            "  %(prog)s showcase                     # all demos\n"
        ),
    )
    parser.add_argument(
        "command",
        choices=["maid", "github", "assistant", "coder", "queen", "showcase"],
        help="demo to run",
    )
    parser.add_argument(
        "task_text",
        nargs="?",
        default=None,
        help="custom task/question text (for assistant, coder, queen)",
    )
    parser.add_argument(
        "--addr",
        default="localhost:50051",
        help="kernel gRPC address (default: localhost:50051)",
    )

    args = parser.parse_args()

    print()
    print("=" * 55)
    print("  HiveKernel Live Demo")
    print("=" * 55)

    success = asyncio.run(main_async(args))
    print()
    if success:
        print("  Demo completed successfully.")
    else:
        print("  Demo finished with errors.")
    sys.exit(0 if success else 1)


if __name__ == "__main__":
    main()
