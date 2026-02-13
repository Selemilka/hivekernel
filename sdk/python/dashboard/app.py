"""
HiveKernel Web Dashboard
========================

FastAPI backend that bridges REST/WebSocket to the Go kernel via gRPC.
Serves a D3.js-based process tree visualization at http://localhost:8080.

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


async def build_tree() -> dict:
    """Fetch king + all children and return the full tree."""
    try:
        king = await stub.GetProcessInfo(
            core_pb2.ProcessInfoRequest(pid=1), metadata=md(1), timeout=5,
        )
        children_resp = await stub.ListChildren(
            core_pb2.ListChildrenRequest(recursive=True), metadata=md(1), timeout=5,
        )
        nodes = [proc_to_dict(king)]
        for c in children_resp.children:
            nodes.append(proc_to_dict(c))
        return {"ok": True, "nodes": nodes}
    except grpc.aio.AioRpcError as e:
        return {"ok": False, "error": f"gRPC: {e.code().name}: {e.details()}"}
    except Exception as e:
        return {"ok": False, "error": str(e)}


async def ws_broadcaster():
    """Background task: poll kernel every 2s and push tree to all WS clients."""
    while True:
        await asyncio.sleep(2)
        if not ws_clients:
            continue
        tree = await build_tree()
        msg = json.dumps({"type": "tree", "data": tree})
        dead = set()
        for ws in ws_clients:
            try:
                await ws.send_text(msg)
            except Exception:
                dead.add(ws)
        ws_clients -= dead


@asynccontextmanager
async def lifespan(app: FastAPI):
    global channel, stub
    channel = grpc.aio.insecure_channel(CORE_ADDR)
    stub = core_pb2_grpc.CoreServiceStub(channel)
    task = asyncio.create_task(ws_broadcaster())
    print(f"Dashboard connecting to kernel at {CORE_ADDR}")
    yield
    task.cancel()
    await channel.close()


app = FastAPI(title="HiveKernel Dashboard", lifespan=lifespan)


# ─── REST API ───

@app.get("/api/tree")
async def api_tree():
    return await build_tree()


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
        # Send initial tree.
        tree = await build_tree()
        await ws.send_text(json.dumps({"type": "tree", "data": tree}))
        # Listen for client messages.
        while True:
            data = await ws.receive_text()
            try:
                msg = json.loads(data)
                if msg.get("type") == "refresh":
                    tree = await build_tree()
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
