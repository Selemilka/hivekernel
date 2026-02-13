"""
End-to-end test: Python SDK -> gRPC -> Go Core.

Requires the Go hivekernel to be running on localhost:50051.
Run: bin/hivekernel.exe --listen :50051
Then: python sdk/python/examples/test_e2e.py
"""

import asyncio
import sys
sys.path.insert(0, "sdk/python")

import grpc.aio
from hivekernel_sdk import agent_pb2, core_pb2, core_pb2_grpc


async def main():
    channel = grpc.aio.insecure_channel("localhost:50051")
    stub = core_pb2_grpc.CoreServiceStub(channel)

    # Use PID 1 (king) as caller for testing.
    md = [("x-hivekernel-pid", "1")]

    # 1. GetProcessInfo for king (PID 1)
    print("--- GetProcessInfo(PID 1) ---")
    info = await stub.GetProcessInfo(core_pb2.ProcessInfoRequest(pid=1), metadata=md)
    print(f"  PID={info.pid} Name={info.name} Role={info.role} Cog={info.cognitive_tier} "
          f"Model={info.model} State={info.state} VPS={info.vps}")

    # 2. ListChildren of king
    print("\n--- ListChildren(king) ---")
    children = await stub.ListChildren(core_pb2.ListChildrenRequest(recursive=True), metadata=md)
    for c in children.children:
        print(f"  PID={c.pid} PPID={c.ppid} Name={c.name} Role={c.role} State={c.state}")

    # 3. SpawnChild under queen (PID 2)
    print("\n--- SpawnChild under queen (PID 2) ---")
    md_queen = [("x-hivekernel-pid", "2")]
    resp = await stub.SpawnChild(
        agent_pb2.SpawnRequest(
            name="test-task",
            role=agent_pb2.ROLE_TASK,
            cognitive_tier=agent_pb2.COG_OPERATIONAL,
            model="mini",
        ),
        metadata=md_queen,
    )
    print(f"  Success={resp.success} ChildPID={resp.child_pid} Error={resp.error}")

    # 4. Verify new process exists
    print("\n--- GetProcessInfo(new child) ---")
    info2 = await stub.GetProcessInfo(
        core_pb2.ProcessInfoRequest(pid=resp.child_pid), metadata=md
    )
    print(f"  PID={info2.pid} PPID={info2.ppid} Name={info2.name} Model={info2.model} "
          f"User={info2.user} VPS={info2.vps}")

    # 5. SendMessage
    print("\n--- SendMessage (queen -> king) ---")
    msg_resp = await stub.SendMessage(
        agent_pb2.SendMessageRequest(
            to_pid=1,
            type="hello",
            priority=agent_pb2.PRIORITY_NORMAL,
            payload=b"Hello from Python SDK!",
        ),
        metadata=md_queen,
    )
    print(f"  Delivered={msg_resp.delivered} MessageID={msg_resp.message_id}")

    # 6. Log
    print("\n--- Log ---")
    await stub.Log(
        agent_pb2.LogRequest(level=agent_pb2.LOG_INFO, message="E2E test passed!"),
        metadata=md_queen,
    )
    print("  Logged OK")

    print("\n=== All Phase 0 E2E tests passed! ===")

    await channel.close()


if __name__ == "__main__":
    asyncio.run(main())
