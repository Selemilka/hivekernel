"""
HiveKernel Web Dashboard
========================

FastAPI backend that bridges REST/WebSocket to the Go kernel via gRPC.
Serves a D3.js-based process tree visualization at http://localhost:8080.

Uses event sourcing: subscribes to kernel's SubscribeEvents stream for
real-time delta updates instead of polling the full tree every 2s.

Usage:
    pip install fastapi uvicorn
    python sdk/python/dashboard/app.py
"""

import asyncio
import json
import os
import sys
from contextlib import asynccontextmanager
from pathlib import Path

import grpc
import grpc.aio
from fastapi import FastAPI, WebSocket, WebSocketDisconnect
from fastapi.responses import JSONResponse
from fastapi.staticfiles import StaticFiles

# Add SDK to path so we can import generated protos.
SDK_DIR = str(Path(__file__).resolve().parent.parent)
if SDK_DIR not in sys.path:
    sys.path.insert(0, SDK_DIR)

from hivekernel_sdk import agent_pb2, core_pb2, core_pb2_grpc  # noqa: E402

CORE_ADDR = os.environ.get("HIVEKERNEL_ADDR", "localhost:50051")
STATIC_DIR = Path(__file__).resolve().parent / "static"

ROLE_NAMES = {0: "kernel", 1: "daemon", 2: "agent", 3: "architect", 4: "lead", 5: "worker", 6: "task"}
TIER_NAMES = {0: "strategic", 1: "tactical", 2: "operational"}
STATE_NAMES = {0: "idle", 1: "running", 2: "blocked", 3: "sleeping", 4: "dead", 5: "zombie"}

# Global state
channel: grpc.aio.Channel = None
stub: core_pb2_grpc.CoreServiceStub = None
ws_clients: set[WebSocket] = set()
tree_cache: dict[int, dict] = {}   # pid -> node dict
last_seq: int = 0


def md(pid: int):
    """Build gRPC metadata for the given caller PID."""
    return [("x-hivekernel-pid", str(pid))]


def proc_to_dict(p) -> dict:
    """Convert a ProcessInfo protobuf to a JSON-friendly dict."""
    return {
        "pid": p.pid,
        "ppid": p.ppid,
        "name": p.name,
        "user": p.user,
        "role": ROLE_NAMES.get(p.role, str(p.role)),
        "role_id": p.role,
        "cognitive_tier": TIER_NAMES.get(p.cognitive_tier, str(p.cognitive_tier)),
        "tier_id": p.cognitive_tier,
        "model": p.model,
        "state": STATE_NAMES.get(p.state, str(p.state)),
        "state_id": p.state,
        "vps": p.vps,
        "tokens_consumed": p.tokens_consumed,
        "context_usage_percent": round(p.context_usage_percent, 1),
        "child_count": p.child_count,
        "started_at": p.started_at,
        "current_task_id": p.current_task_id,
        "runtime_addr": p.runtime_addr,
    }


async def full_resync():
    """Fetch full tree from kernel and rebuild tree_cache."""
    global tree_cache
    try:
        king = await stub.GetProcessInfo(
            core_pb2.ProcessInfoRequest(pid=1), metadata=md(1), timeout=5,
        )
        children_resp = await stub.ListChildren(
            core_pb2.ListChildrenRequest(recursive=True), metadata=md(1), timeout=5,
        )
        new_cache = {}
        king_dict = proc_to_dict(king)
        new_cache[king_dict["pid"]] = king_dict
        for c in children_resp.children:
            d = proc_to_dict(c)
            new_cache[d["pid"]] = d
        tree_cache = new_cache
        return True
    except Exception as e:
        print(f"[dashboard] full_resync failed: {e}")
        return False


def get_tree_response() -> dict:
    """Build tree response from cache."""
    if not tree_cache:
        return {"ok": False, "error": "no data yet"}
    return {"ok": True, "nodes": list(tree_cache.values())}


def apply_event(event) -> dict | None:
    """Apply a ProcessEvent to tree_cache and return a delta dict for WS clients."""
    global last_seq
    last_seq = event.seq

    if event.type == "spawned":
        node = {
            "pid": event.pid,
            "ppid": event.ppid,
            "name": event.name,
            "user": "",
            "role": event.role,
            "role_id": 0,
            "cognitive_tier": event.tier,
            "tier_id": 0,
            "model": event.model,
            "state": event.state or "idle",
            "state_id": 0,
            "vps": "",
            "tokens_consumed": 0,
            "context_usage_percent": 0,
            "child_count": 0,
            "started_at": 0,
            "current_task_id": "",
            "runtime_addr": "",
        }
        tree_cache[event.pid] = node
        # Update parent's child_count.
        if event.ppid in tree_cache:
            parent = tree_cache[event.ppid]
            parent["child_count"] = parent.get("child_count", 0) + 1
        return {"action": "add", "node": node}

    elif event.type == "state_changed":
        if event.pid in tree_cache:
            node = tree_cache[event.pid]
            node["state"] = event.new_state
            return {"action": "update", "pid": event.pid, "state": event.new_state}

    elif event.type == "removed":
        removed = tree_cache.pop(event.pid, None)
        if removed and removed.get("ppid") in tree_cache:
            parent = tree_cache[removed["ppid"]]
            cc = parent.get("child_count", 1)
            parent["child_count"] = max(0, cc - 1)
        return {"action": "remove", "pid": event.pid}

    elif event.type == "log":
        name = event.name
        if not name and event.pid in tree_cache:
            name = tree_cache[event.pid].get("name", "")
        return {
            "action": "log",
            "pid": event.pid,
            "name": name,
            "level": event.level,
            "message": event.message,
            "ts": event.timestamp_ms,
        }

    return None


def make_lifecycle_log(event) -> dict | None:
    """Generate a synthetic log entry for lifecycle events."""
    ts = event.timestamp_ms
    if event.type == "spawned":
        return {"action": "log", "pid": event.pid, "name": event.name,
                "level": "info", "message": f"Process spawned (role={event.role}, model={event.model})", "ts": ts}
    elif event.type == "state_changed":
        name = tree_cache.get(event.pid, {}).get("name", "")
        return {"action": "log", "pid": event.pid, "name": name,
                "level": "info", "message": f"State: {event.old_state} -> {event.new_state}", "ts": ts}
    elif event.type == "removed":
        return {"action": "log", "pid": event.pid, "name": "",
                "level": "info", "message": "Process removed (reaped)", "ts": ts}
    return None


async def broadcast_delta(delta: dict):
    """Send a delta message to all connected WS clients."""
    global ws_clients
    msg = json.dumps({"type": "delta", "data": delta})
    dead = set()
    for ws in ws_clients:
        try:
            await ws.send_text(msg)
        except Exception:
            dead.add(ws)
    ws_clients -= dead


async def event_listener():
    """Subscribe to kernel event stream, apply deltas, push to WS clients."""
    global last_seq
    while True:
        try:
            # Do a full resync to populate cache before subscribing.
            await full_resync()
            print(f"[dashboard] subscribing to events since seq {last_seq}")
            stream = stub.SubscribeEvents(
                core_pb2.SubscribeEventsRequest(since_seq=last_seq),
                metadata=md(1),
            )
            async for event in stream:
                delta = apply_event(event)
                if delta:
                    await broadcast_delta(delta)
                # Also send lifecycle events as log entries.
                if event.type in ("spawned", "state_changed", "removed"):
                    log_delta = make_lifecycle_log(event)
                    if log_delta:
                        await broadcast_delta(log_delta)
        except grpc.aio.AioRpcError as e:
            print(f"[dashboard] event stream error: {e.code().name}: {e.details()}")
            await asyncio.sleep(2)
        except Exception as e:
            print(f"[dashboard] event listener error: {e}")
            await asyncio.sleep(2)


async def safety_resync():
    """Periodic full resync every 30s as safety net (compare vs gRPC truth)."""
    global ws_clients
    while True:
        await asyncio.sleep(30)
        if not tree_cache:
            continue
        try:
            king = await stub.GetProcessInfo(
                core_pb2.ProcessInfoRequest(pid=1), metadata=md(1), timeout=5,
            )
            children_resp = await stub.ListChildren(
                core_pb2.ListChildrenRequest(recursive=True), metadata=md(1), timeout=5,
            )
            truth_pids = {1}
            for c in children_resp.children:
                truth_pids.add(c.pid)
            cache_pids = set(tree_cache.keys())
            if truth_pids != cache_pids:
                print(f"[dashboard] DRIFT detected: cache={len(cache_pids)} truth={len(truth_pids)}, resyncing")
                await full_resync()
                # Push full tree to all clients.
                msg = json.dumps({"type": "tree", "data": get_tree_response()})
                dead = set()
                for ws in ws_clients:
                    try:
                        await ws.send_text(msg)
                    except Exception:
                        dead.add(ws)
                ws_clients -= dead
        except Exception:
            pass


@asynccontextmanager
async def lifespan(app: FastAPI):
    global channel, stub
    channel = grpc.aio.insecure_channel(CORE_ADDR)
    stub = core_pb2_grpc.CoreServiceStub(channel)
    listener_task = asyncio.create_task(event_listener())
    resync_task = asyncio.create_task(safety_resync())
    print(f"Dashboard connecting to kernel at {CORE_ADDR}")
    yield
    listener_task.cancel()
    resync_task.cancel()
    await channel.close()


app = FastAPI(title="HiveKernel Dashboard", lifespan=lifespan)


# ─── REST API ───

@app.get("/api/tree")
async def api_tree():
    if tree_cache:
        return get_tree_response()
    # Fallback: fetch directly.
    await full_resync()
    return get_tree_response()


@app.get("/api/process/{pid}")
async def api_process(pid: int):
    try:
        info = await stub.GetProcessInfo(
            core_pb2.ProcessInfoRequest(pid=pid), metadata=md(1), timeout=5,
        )
        return {"ok": True, "process": proc_to_dict(info)}
    except grpc.aio.AioRpcError as e:
        return JSONResponse({"ok": False, "error": e.details()}, status_code=400)


@app.post("/api/spawn")
async def api_spawn(body: dict):
    parent_pid = body.get("parent_pid", 1)
    name = body.get("name", "new-agent")
    role_str = body.get("role", "worker")
    tier_str = body.get("tier", "operational")
    runtime_image = body.get("runtime_image", "")
    system_prompt = body.get("system_prompt", "")
    model = body.get("model", "")

    role_map = {v: k for k, v in ROLE_NAMES.items()}
    tier_map = {v: k for k, v in TIER_NAMES.items()}

    try:
        resp = await stub.SpawnChild(
            agent_pb2.SpawnRequest(
                name=name,
                role=role_map.get(role_str, 5),
                cognitive_tier=tier_map.get(tier_str, 2),
                model=model,
                system_prompt=system_prompt,
                runtime_type=agent_pb2.RUNTIME_PYTHON,
                runtime_image=runtime_image,
            ),
            metadata=md(parent_pid),
            timeout=30,
        )
        if resp.success:
            return {"ok": True, "child_pid": resp.child_pid}
        return JSONResponse({"ok": False, "error": resp.error}, status_code=400)
    except grpc.aio.AioRpcError as e:
        return JSONResponse({"ok": False, "error": e.details()}, status_code=500)


@app.post("/api/kill/{pid}")
async def api_kill(pid: int):
    try:
        # Look up target's PPID so we can call kill as the parent.
        info = await stub.GetProcessInfo(
            core_pb2.ProcessInfoRequest(pid=pid), metadata=md(1), timeout=5,
        )
        parent_pid = info.ppid
        resp = await stub.KillChild(
            agent_pb2.KillRequest(target_pid=pid, recursive=True),
            metadata=md(parent_pid),
            timeout=10,
        )
        if resp.success:
            return {"ok": True, "killed_pids": list(resp.killed_pids)}
        return JSONResponse({"ok": False, "error": resp.error}, status_code=400)
    except grpc.aio.AioRpcError as e:
        return JSONResponse({"ok": False, "error": e.details()}, status_code=500)


@app.post("/api/execute/{pid}")
async def api_execute(pid: int, body: dict):
    description = body.get("description", "Dashboard task")
    params = body.get("params", {})
    timeout = body.get("timeout", 60)

    try:
        resp = await stub.ExecuteTask(
            core_pb2.ExecuteTaskRequest(
                target_pid=pid,
                description=description,
                params=params,
                timeout_seconds=timeout,
            ),
            metadata=md(1),
            timeout=timeout + 10,
        )
        if resp.success:
            result = {
                "exit_code": resp.result.exit_code,
                "output": resp.result.output,
                "artifacts": dict(resp.result.artifacts),
                "metadata": dict(resp.result.metadata),
            }
            return {"ok": True, "result": result}
        return JSONResponse({"ok": False, "error": resp.error}, status_code=400)
    except grpc.aio.AioRpcError as e:
        return JSONResponse({"ok": False, "error": e.details()}, status_code=500)


@app.post("/api/run-task")
async def api_run_task(body: dict):
    """Delegate a task to Queen (the local coordinator daemon)."""
    task_text = body.get("task", "")
    max_workers = str(body.get("max_workers", 3))
    timeout = body.get("timeout", 300)

    if not task_text.strip():
        return JSONResponse({"ok": False, "error": "Task description is empty"}, status_code=400)

    # Find Queen PID: daemon child of King (PID 1) with name starting with "queen@".
    queen_pid = _find_queen_pid()
    if queen_pid is None:
        return JSONResponse({"ok": False, "error": "Queen not found in process tree"}, status_code=503)

    # Execute task on Queen — she decides the strategy (simple vs complex).
    try:
        exec_resp = await stub.ExecuteTask(
            core_pb2.ExecuteTaskRequest(
                target_pid=queen_pid,
                description=task_text,
                params={"task": task_text, "max_workers": max_workers},
                timeout_seconds=timeout,
            ),
            metadata=md(1),
            timeout=timeout + 30,
        )
        if exec_resp.success:
            result = {
                "exit_code": exec_resp.result.exit_code,
                "output": exec_resp.result.output,
                "artifacts": dict(exec_resp.result.artifacts),
                "metadata": dict(exec_resp.result.metadata),
            }
            return {"ok": True, "queen_pid": queen_pid, "result": result}
        return JSONResponse(
            {"ok": False, "error": exec_resp.error, "queen_pid": queen_pid},
            status_code=400,
        )
    except grpc.aio.AioRpcError as e:
        return JSONResponse(
            {"ok": False, "error": f"Execute error: {e.details()}", "queen_pid": queen_pid},
            status_code=500,
        )


def _find_queen_pid() -> int | None:
    """Find Queen's PID from tree_cache (daemon child of King named 'queen@...')."""
    for pid, node in tree_cache.items():
        if node.get("ppid") == 1 and node.get("role") == "daemon" and node.get("name", "").startswith("queen@"):
            return pid
    return None


@app.get("/api/artifacts")
async def api_artifacts():
    try:
        resp = await stub.ListArtifacts(
            core_pb2.ListArtifactsRequest(prefix=""), metadata=md(1), timeout=5,
        )
        artifacts = []
        for a in resp.artifacts:
            artifacts.append({
                "artifact_id": a.artifact_id,
                "key": a.key,
                "content_type": a.content_type,
                "size_bytes": a.size_bytes,
                "stored_by_pid": a.stored_by_pid,
                "stored_at": a.stored_at,
            })
        return {"ok": True, "artifacts": artifacts}
    except grpc.aio.AioRpcError as e:
        return JSONResponse({"ok": False, "error": e.details()}, status_code=500)


# ─── WebSocket ───

@app.websocket("/ws")
async def websocket_endpoint(ws: WebSocket):
    await ws.accept()
    ws_clients.add(ws)
    try:
        # Send initial tree from cache.
        if tree_cache:
            tree = get_tree_response()
        else:
            await full_resync()
            tree = get_tree_response()
        await ws.send_text(json.dumps({"type": "tree", "data": tree}))
        # Listen for client messages.
        while True:
            data = await ws.receive_text()
            try:
                msg = json.loads(data)
                if msg.get("type") == "refresh":
                    await full_resync()
                    tree = get_tree_response()
                    await ws.send_text(json.dumps({"type": "tree", "data": tree}))
            except json.JSONDecodeError:
                pass
    except WebSocketDisconnect:
        pass
    finally:
        ws_clients.discard(ws)


# ─── Static files (must be last) ───

app.mount("/", StaticFiles(directory=str(STATIC_DIR), html=True), name="static")


if __name__ == "__main__":
    import uvicorn
    port = int(os.environ.get("DASHBOARD_PORT", "8080"))
    print(f"Starting HiveKernel Dashboard on http://localhost:{port}")
    print(f"Kernel address: {CORE_ADDR}")
    uvicorn.run(app, host="0.0.0.0", port=port, log_level="info")
