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
from fastapi.responses import JSONResponse, RedirectResponse
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

# Task history (in-memory, max 500 entries).
task_history: list[dict] = []
TASK_HISTORY_MAX = 500

# Message log (in-memory, max 500 entries).
message_log: list[dict] = []
MESSAGE_LOG_MAX = 500

# Trace log (in-memory, trace_id -> list of events).
trace_log: dict[str, list[dict]] = {}
MAX_TRACES = 100


def record_task(pid: int, name: str, description: str, result: str,
                success: bool, error: str, duration_ms: int):
    """Record a completed task in the history."""
    import time as _time
    entry = {
        "pid": pid,
        "name": name,
        "description": description,
        "result": result,
        "success": success,
        "error": error,
        "duration_ms": duration_ms,
        "timestamp": int(_time.time() * 1000),
    }
    task_history.append(entry)
    # Trim oldest entries.
    while len(task_history) > TASK_HISTORY_MAX:
        task_history.pop(0)


def record_message(from_pid: int, to_pid: int, from_name: str, to_name: str,
                    msg_type: str, reply_to: str, payload_preview: str,
                    trace_id: str = "", trace_span: str = ""):
    """Record an IPC message event."""
    import time as _time
    entry = {
        "from_pid": from_pid,
        "to_pid": to_pid,
        "from_name": from_name,
        "to_name": to_name,
        "type": msg_type,
        "reply_to": reply_to,
        "payload_preview": payload_preview[:2000],
        "timestamp": int(_time.time() * 1000),
        "trace_id": trace_id,
        "trace_span": trace_span,
    }
    message_log.append(entry)
    while len(message_log) > MESSAGE_LOG_MAX:
        message_log.pop(0)
    return entry


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

    # Record event in trace_log if it has a trace_id.
    evt_trace_id = getattr(event, "trace_id", "") or ""
    evt_trace_span = getattr(event, "trace_span", "") or ""
    if evt_trace_id:
        import time as _time
        trace_entry = {
            "seq": event.seq,
            "timestamp_ms": event.timestamp_ms,
            "type": event.type,
            "pid": event.pid,
            "ppid": event.ppid,
            "name": event.name,
            "trace_id": evt_trace_id,
            "trace_span": evt_trace_span,
        }
        if event.type == "message_sent":
            trace_entry["message_type"] = event.message
            trace_entry["reply_to"] = getattr(event, "reply_to", "")
        elif event.type == "state_changed":
            trace_entry["old_state"] = event.old_state
            trace_entry["new_state"] = event.new_state
        elif event.type == "log":
            trace_entry["level"] = event.level
            trace_entry["message"] = event.message
        if evt_trace_id not in trace_log:
            # Evict oldest trace if at capacity.
            if len(trace_log) >= MAX_TRACES:
                oldest_key = next(iter(trace_log))
                del trace_log[oldest_key]
            trace_log[evt_trace_id] = []
        trace_log[evt_trace_id].append(trace_entry)

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

    elif event.type == "message_delivered":
        # event.pid = to_pid, event.ppid = from_pid
        message_id = getattr(event, "message_id", "") or ""
        import time as _time
        # Update matching message_log entry with delivered_at.
        for entry in reversed(message_log):
            if entry.get("message_id") == message_id and message_id:
                entry["delivered_at"] = int(_time.time() * 1000)
                entry["state"] = "delivered"
                break
        return {
            "action": "message_delivered",
            "message_id": message_id,
            "to_pid": event.pid,
            "from_pid": event.ppid,
            "ts": event.timestamp_ms,
        }

    elif event.type == "message_sent":
        # event.pid = from_pid, event.ppid = to_pid,
        # event.name = from_name, event.role = to_name, event.message = msg_type
        from_pid = event.pid
        to_pid = event.ppid
        from_name = event.name
        to_name = event.role
        msg_type = event.message
        reply_to = getattr(event, "reply_to", "") or ""
        payload_preview = getattr(event, "payload_preview", "") or ""
        message_id = getattr(event, "message_id", "") or ""

        # Record in message log.
        entry = record_message(from_pid, to_pid, from_name, to_name,
                               msg_type, reply_to, payload_preview,
                               trace_id=evt_trace_id, trace_span=evt_trace_span)
        entry["message_id"] = message_id
        entry["state"] = "sent"

        # Record task_response as a delegation result in task_history.
        if msg_type == "task_response" and payload_preview:
            result_text = payload_preview
            try:
                data = json.loads(payload_preview)
                result_text = data.get("output", data.get("error", payload_preview))
            except (json.JSONDecodeError, ValueError):
                pass
            success = True
            error_text = ""
            try:
                data = json.loads(payload_preview)
                if data.get("error"):
                    success = False
                    error_text = data["error"]
            except (json.JSONDecodeError, ValueError):
                pass
            record_task(
                pid=to_pid,
                name=to_name or f"PID {to_pid}",
                description=f"Async result from {from_name or f'PID {from_pid}'}",
                result=result_text[:4000],
                success=success,
                error=error_text,
                duration_ms=0,
            )

        return {
            "action": "message",
            "from_pid": from_pid,
            "to_pid": to_pid,
            "from_name": from_name,
            "to_name": to_name,
            "type": msg_type,
            "reply_to": reply_to,
            "payload_preview": payload_preview,
            "ts": event.timestamp_ms,
            "trace_id": evt_trace_id,
            "trace_span": evt_trace_span,
            "message_id": message_id,
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
    import time as _time
    description = body.get("description", "Dashboard task")
    params = body.get("params", {})
    timeout = body.get("timeout", 60)
    node_name = tree_cache.get(pid, {}).get("name", "")
    start = _time.monotonic()

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
        duration_ms = int((_time.monotonic() - start) * 1000)
        if resp.success:
            result = {
                "exit_code": resp.result.exit_code,
                "output": resp.result.output,
                "artifacts": dict(resp.result.artifacts),
                "metadata": dict(resp.result.metadata),
            }
            record_task(pid, node_name, description, resp.result.output,
                        True, "", duration_ms)
            return {"ok": True, "result": result}
        record_task(pid, node_name, description, "", False, resp.error, duration_ms)
        return JSONResponse({"ok": False, "error": resp.error}, status_code=400)
    except grpc.aio.AioRpcError as e:
        duration_ms = int((_time.monotonic() - start) * 1000)
        record_task(pid, node_name, description, "", False, e.details(), duration_ms)
        return JSONResponse({"ok": False, "error": e.details()}, status_code=500)


@app.post("/api/run-task")
async def api_run_task(body: dict):
    """Delegate a task to Queen (the local coordinator daemon)."""
    import time as _time
    task_text = body.get("task", "")
    max_workers = str(body.get("max_workers", 3))
    timeout = body.get("timeout", 300)

    if not task_text.strip():
        return JSONResponse({"ok": False, "error": "Task description is empty"}, status_code=400)

    # Find Queen PID: daemon child of King (PID 1) with name starting with "queen@".
    queen_pid = _find_queen_pid()
    if queen_pid is None:
        return JSONResponse({"ok": False, "error": "Queen not found in process tree"}, status_code=503)

    queen_name = tree_cache.get(queen_pid, {}).get("name", "queen")
    start = _time.monotonic()

    # Execute task on Queen -- she decides the strategy (simple vs complex).
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
        duration_ms = int((_time.monotonic() - start) * 1000)
        if exec_resp.success:
            result = {
                "exit_code": exec_resp.result.exit_code,
                "output": exec_resp.result.output,
                "artifacts": dict(exec_resp.result.artifacts),
                "metadata": dict(exec_resp.result.metadata),
            }
            record_task(queen_pid, queen_name, task_text,
                        exec_resp.result.output, True, "", duration_ms)
            return {"ok": True, "queen_pid": queen_pid, "result": result}
        record_task(queen_pid, queen_name, task_text, "",
                    False, exec_resp.error, duration_ms)
        return JSONResponse(
            {"ok": False, "error": exec_resp.error, "queen_pid": queen_pid},
            status_code=400,
        )
    except grpc.aio.AioRpcError as e:
        duration_ms = int((_time.monotonic() - start) * 1000)
        record_task(queen_pid, queen_name, task_text, "",
                    False, e.details(), duration_ms)
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


def _find_agent_pid(name_prefix: str) -> int | None:
    """Find an agent's PID from tree_cache by name prefix."""
    for pid, node in tree_cache.items():
        if node.get("name", "").startswith(name_prefix):
            return pid
    return None


def _get_sibling_pids() -> dict[str, int]:
    """Build a map of known daemon names to PIDs from tree_cache."""
    result = {}
    for pid, node in tree_cache.items():
        name = node.get("name", "")
        if node.get("role") == "daemon" and name:
            result[name] = pid
    return result


@app.post("/api/chat")
async def api_chat(body: dict):
    """Send a message to the Assistant agent, return response."""
    import time as _time
    message = body.get("message", "")
    history = body.get("history", [])

    if not message.strip():
        return JSONResponse({"ok": False, "error": "Empty message"}, status_code=400)

    assistant_pid = _find_agent_pid("assistant")
    if assistant_pid is None:
        return JSONResponse({"ok": False, "error": "Assistant agent not found"}, status_code=503)

    sibling_pids = _get_sibling_pids()
    start = _time.monotonic()

    try:
        resp = await stub.ExecuteTask(
            core_pb2.ExecuteTaskRequest(
                target_pid=assistant_pid,
                description=message,
                params={
                    "message": message,
                    "history": json.dumps(history, ensure_ascii=False),
                    "sibling_pids": json.dumps(sibling_pids),
                },
                timeout_seconds=60,
            ),
            metadata=md(1),
            timeout=70,
        )
        duration_ms = int((_time.monotonic() - start) * 1000)
        if resp.success:
            record_task(assistant_pid, "assistant", message,
                        resp.result.output, True, "", duration_ms)
            return {
                "ok": True,
                "response": resp.result.output,
                "exit_code": resp.result.exit_code,
            }
        record_task(assistant_pid, "assistant", message, "",
                    False, resp.error, duration_ms)
        return JSONResponse({"ok": False, "error": resp.error}, status_code=400)
    except grpc.aio.AioRpcError as e:
        duration_ms = int((_time.monotonic() - start) * 1000)
        record_task(assistant_pid, "assistant", message, "",
                    False, e.details(), duration_ms)
        return JSONResponse({"ok": False, "error": e.details()}, status_code=500)


@app.get("/api/cron")
async def api_cron_list():
    """List all cron entries."""
    try:
        resp = await stub.ListCron(core_pb2.ListCronRequest(), metadata=md(1), timeout=5)
        entries = []
        for e in resp.entries:
            entries.append({
                "id": e.id,
                "name": e.name,
                "cron_expression": e.cron_expression,
                "action": e.action,
                "target_pid": e.target_pid,
                "execute_description": e.execute_description,
                "enabled": e.enabled,
                "last_run_ms": e.last_run_ms,
                "next_run_ms": e.next_run_ms,
            })
        return {"ok": True, "entries": entries}
    except grpc.aio.AioRpcError as e:
        return JSONResponse({"ok": False, "error": e.details()}, status_code=500)


@app.post("/api/cron")
async def api_cron_add(body: dict):
    """Add a cron entry."""
    try:
        resp = await stub.AddCron(
            core_pb2.AddCronRequest(
                name=body.get("name", ""),
                cron_expression=body.get("cron_expression", ""),
                action=body.get("action", "execute"),
                target_pid=body.get("target_pid", 0),
                execute_description=body.get("description", ""),
                execute_params=body.get("params", {}),
            ),
            metadata=md(1),
            timeout=5,
        )
        if resp.error:
            return JSONResponse({"ok": False, "error": resp.error}, status_code=400)
        return {"ok": True, "cron_id": resp.cron_id}
    except grpc.aio.AioRpcError as e:
        return JSONResponse({"ok": False, "error": e.details()}, status_code=500)


@app.delete("/api/cron/{cron_id}")
async def api_cron_remove(cron_id: str):
    """Remove a cron entry."""
    try:
        resp = await stub.RemoveCron(
            core_pb2.RemoveCronRequest(cron_id=cron_id), metadata=md(1), timeout=5,
        )
        if resp.error:
            return JSONResponse({"ok": False, "error": resp.error}, status_code=400)
        return {"ok": True}
    except grpc.aio.AioRpcError as e:
        return JSONResponse({"ok": False, "error": e.details()}, status_code=500)


@app.get("/api/task-history")
async def api_task_history(limit: int = 50, pid: int = 0):
    """Return recent task history, optionally filtered by PID."""
    filtered = task_history
    if pid > 0:
        filtered = [t for t in task_history if t["pid"] == pid]
    # Return most recent first, capped at limit.
    return {"ok": True, "entries": list(reversed(filtered[-limit:]))}


@app.post("/api/send-message")
async def api_send_message(body: dict):
    """Send a message to an agent's inbox. Dashboard sends as PID 1 (king)."""
    to_pid = body.get("to_pid", 0)
    msg_type = body.get("type", "task_request")
    payload = body.get("payload", "")

    if not to_pid:
        return JSONResponse({"ok": False, "error": "to_pid is required"}, status_code=400)

    try:
        resp = await stub.SendMessage(
            agent_pb2.SendMessageRequest(
                to_pid=to_pid,
                type=msg_type,
                payload=payload.encode("utf-8") if isinstance(payload, str) else payload,
                priority=agent_pb2.PRIORITY_NORMAL,
            ),
            metadata=md(1),
            timeout=10,
        )
        if resp.delivered:
            return {"ok": True, "message_id": resp.message_id}
        return JSONResponse({"ok": False, "error": resp.error}, status_code=400)
    except grpc.aio.AioRpcError as e:
        return JSONResponse({"ok": False, "error": e.details()}, status_code=500)


@app.get("/api/messages")
async def api_messages(pid: int = 0, limit: int = 50):
    """Return recent IPC messages, optionally filtered by PID (from or to)."""
    filtered = message_log
    if pid > 0:
        filtered = [m for m in message_log if m["from_pid"] == pid or m["to_pid"] == pid]
    return {"ok": True, "entries": list(reversed(filtered[-limit:]))}


@app.get("/api/inbox/{pid}")
async def api_inbox(pid: int):
    """Return messages currently queued in a process's kernel inbox."""
    try:
        resp = await stub.ListInbox(
            core_pb2.ListInboxRequest(pid=pid), metadata=md(1), timeout=5,
        )
        messages = []
        for m in resp.messages:
            messages.append({
                "message_id": m.message_id,
                "from_pid": m.from_pid,
                "from_name": m.from_name,
                "type": m.type,
                "priority": int(m.priority),
                "payload_preview": m.payload[:200].decode("utf-8", errors="replace") if m.payload else "",
                "requires_ack": m.requires_ack,
                "reply_to": m.reply_to,
            })
        return {"ok": True, "messages": messages, "total": resp.total}
    except grpc.aio.AioRpcError as e:
        return JSONResponse({"ok": False, "error": e.details()}, status_code=500)


@app.get("/api/traces")
async def api_traces():
    """List recent traces with summary info."""
    summaries = []
    for tid, events in trace_log.items():
        if not events:
            continue
        first_ts = events[0].get("timestamp_ms", 0)
        last_ts = events[-1].get("timestamp_ms", 0)
        initiator_pid = events[0].get("pid", 0)
        summaries.append({
            "trace_id": tid,
            "event_count": len(events),
            "first_ts": first_ts,
            "last_ts": last_ts,
            "initiator_pid": initiator_pid,
        })
    # Most recent first.
    summaries.sort(key=lambda s: s["last_ts"], reverse=True)
    return {"ok": True, "traces": summaries}


@app.get("/api/traces/{trace_id}")
async def api_trace_detail(trace_id: str):
    """Get all events for a specific trace, sorted by timestamp."""
    events = trace_log.get(trace_id)
    if events is None:
        return JSONResponse({"ok": False, "error": "trace not found"}, status_code=404)
    sorted_events = sorted(events, key=lambda e: e.get("timestamp_ms", 0))
    return {"ok": True, "trace_id": trace_id, "events": sorted_events}


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


# ─── Convenience redirects ───

@app.get("/chat")
async def redirect_chat():
    return RedirectResponse("/chat.html")

@app.get("/tree")
async def redirect_tree():
    return RedirectResponse("/index.html")

@app.get("/cron")
async def redirect_cron():
    return RedirectResponse("/cron.html")


# ─── Static files (must be last) ───

app.mount("/", StaticFiles(directory=str(STATIC_DIR), html=True), name="static")


if __name__ == "__main__":
    import uvicorn
    port = int(os.environ.get("DASHBOARD_PORT", "8080"))
    print(f"Starting HiveKernel Dashboard on http://localhost:{port}")
    print(f"Kernel address: {CORE_ADDR}")
    uvicorn.run(app, host="0.0.0.0", port=port, log_level="info")
