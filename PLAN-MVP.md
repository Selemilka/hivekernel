# HiveKernel MVP Plan — Phases 8-10

## Goal
Working kernel + Python agents end-to-end: spawn, communicate, delegate tasks, hierarchy.

---

## Phase 8 — Wire RuntimeImage (spawn chain)

Problem: proto defines runtime_type/runtime_image, Process.SpawnRequest has them,
but syscall_handler, grpc_core, Python SDK don't pass them. Chain is broken at entry points.

Tasks:
1. syscall_handler.go handleSpawn — add RuntimeType and RuntimeImage to SpawnRequest
2. grpc_core.go SpawnChild — same
3. syscall.py spawn() — add runtime_type, runtime_image params
4. client.py spawn_child() — same
5. main.go — normalize coreAddr (:50051 -> localhost:50051)
6. agent.py Init handler — update agent._core.pid = request.pid after Init
7. main.go demo — remove manual rtManager.StartRuntime(), use RuntimeImage

Verify: go test ./internal/... && go build

---

## Phase 9 — End-to-End integration test

Problem: never tested real chain kernel -> Python process -> gRPC -> task -> result.

Tasks:
1. Create sdk/python/examples/sub_worker.py — minimal echo agent for auto-spawn
2. Create sdk/python/examples/test_runtime_e2e.py — E2E test:
   - Connect to kernel
   - Spawn agent with runtime_image
   - Kernel launches Python process
   - Send task via Executor, verify result
   - Spawn second agent, first does execute_on on second
   - Kill -> verify process stopped
3. PYTHONPATH setup so runner.py finds modules from examples/

Verify: start kernel + run test, full pipeline works

---

## Phase 10 — Multi-agent MVP scenario

Problem: no real scenario with multiple agents demonstrating all functionality.

Tasks:
1. Create sdk/python/examples/team_scenario.py — "team" scenario:
   - Manager (lead) — gets task, splits into subtasks
   - Worker (task) — executes subtask, returns result
   - Manager spawns 2-3 workers via ctx.spawn(runtime_image=...)
   - Delegates subtasks via ctx.execute_on()
   - Collects results, merges
   - Kills workers, returns final result
2. Verify IPC: workers send messages to manager via ctx.send()
3. Verify artifacts: workers store via ctx.store_artifact(), manager reads via ctx.get_artifact()
4. Verify escalation: worker escalates a problem
5. Update test_integration.py — add runtime spawn checks

Verify: full scenario works, all syscalls exercised
