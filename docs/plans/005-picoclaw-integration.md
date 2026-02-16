# Plan: HiveKernel + PicoClaw Integration (Hybrid Approach)

## Context

HiveKernel — Go-ядро для оркестрации LLM-агентов (дерево процессов, IPC, ресурсы). Агенты подключаются через gRPC AgentService. Сейчас поддерживаются только Python-агенты.

PicoClaw — ультралёгкий Go AI-ассистент (<10MB RAM) с agent loop, 20+ tools (I2C, SPI, web, shell), multi-channel gateway (Telegram, Discord, Slack), и системой подагентов.

**Цель**: PicoClaw-инстансы работают как полноценные процессы в дереве HiveKernel. Главный PicoClaw (queen) может спавнить worker PicoClaw через syscalls ядра. Python-агенты и Claw-агенты сосуществуют в одном дереве.

**Ключевой принцип**: слабая привязка через gRPC AgentService контракт. HiveKernel не импортирует код PicoClaw. PicoClaw не зависит от HiveKernel (кроме proto-файла для hive-режима).

---

## Phase 1: HiveKernel — Runtime Dispatch по типу

### 1.1 Рефакторинг `manager.go` — разделение spawnReal

**File**: `F:/AI/Claude/HiveKernel/internal/runtime/manager.go`

Текущий `spawnReal` (строки 112-229) захардкожен на Python. Разделяем на три метода:

**a) Новый `Manager` struct field + setter:**
```go
// Добавить поле:
clawBin string  // path to picoclaw binary

// Добавить метод:
func (m *Manager) SetClawBin(path string) { m.clawBin = path }
```

**b) Рефакторинг `spawnReal` в диспетчер:**
```go
func (m *Manager) spawnReal(proc *process.Process, rtType RuntimeType) (*AgentRuntime, error) {
    runtimeImage := proc.RuntimeAddr
    if runtimeImage == "" {
        return nil, fmt.Errorf("no RuntimeImage for PID %d", proc.PID)
    }
    switch rtType {
    case RuntimePython:
        return m.spawnPython(proc, runtimeImage)
    case RuntimeClaw:
        return m.spawnClaw(proc, runtimeImage)
    default:
        return nil, fmt.Errorf("unsupported runtime type %q for PID %d", rtType, proc.PID)
    }
}
```

**c) `spawnPython`** — текущее тело `spawnReal` (строки 112-229), без изменений, только переименовать.

**d) Новый `spawnClaw`:**
```go
func (m *Manager) spawnClaw(proc *process.Process, runtimeImage string) (*AgentRuntime, error) {
    clawBin := m.resolveClawBinary(runtimeImage)
    cmd := exec.Command(clawBin, "hive", "--core", m.coreAddr)
    // env: HIVEKERNEL_PID, HIVEKERNEL_CORE + claw-specific env из proc metadata
    return m.launchAndConnect(proc, RuntimeClaw, cmd)
}
```

**e) Выделить общую логику в `launchAndConnect`:**

Из `spawnPython`/`spawnClaw` выносим общий код:
- stdout pipe + cmd.Start()
- ожидание READY `<port>` (10s timeout)
- grpc.NewClient + Init RPC
- создание AgentRuntime + watchProcessExit goroutine

**f) Обогатить InitRequest:**

Сейчас Init передаёт только `Name` в Config (строка 196). Расширяем:
```go
client.Init(ctx, &pb.InitRequest{
    Pid:  proc.PID,
    Ppid: proc.PPID,
    User: proc.User,
    Role: roleToProto(proc.Role),
    CognitiveTier: cogToProto(proc.CognitiveTier),
    Config: &pb.AgentConfig{
        Name:         proc.Name,
        Model:        proc.Model,
        SystemPrompt: proc.SystemPrompt, // нужно добавить поле
        Metadata:     proc.Metadata,     // нужно добавить поле
    },
    Limits: limitsToProto(proc.Limits),
})
```

### 1.2 Добавить SystemPrompt и Metadata в Process

**File**: `F:/AI/Claude/HiveKernel/internal/process/types.go`

Добавить в struct `Process`:
```go
SystemPrompt string
Metadata     map[string]string  // claw_config, env vars, etc.
```

**File**: `F:/AI/Claude/HiveKernel/internal/process/spawner.go`

В `Spawn()` — копировать SystemPrompt и Metadata из SpawnRequest в Process:
```go
proc := &Process{
    // ... existing fields ...
    SystemPrompt: req.SystemPrompt,
    Metadata:     req.Metadata,
}
```

Добавить `Metadata map[string]string` в `SpawnRequest` struct.

### 1.3 Startup config: ClawConfig

**File**: `F:/AI/Claude/HiveKernel/internal/kernel/startup.go`

Добавить:
```go
type ClawAgentConfig struct {
    Workspace           string            `json:"workspace,omitempty"`
    Provider            string            `json:"provider,omitempty"`
    MaxToolIterations   int               `json:"max_tool_iterations,omitempty"`
    RestrictToWorkspace bool              `json:"restrict_to_workspace,omitempty"`
    Channels            []string          `json:"channels,omitempty"`
    Env                 map[string]string `json:"env,omitempty"`
}
```

Добавить поле в `StartupAgent`:
```go
ClawConfig *ClawAgentConfig `json:"claw_config,omitempty"`
```

### 1.4 main.go — передача ClawConfig

**File**: `F:/AI/Claude/HiveKernel/cmd/hivekernel/main.go`

- Добавить флаг `--claw-bin`
- Вызвать `rtManager.SetClawBin(*clawBin)`
- В `spawnStartupAgents`: конвертировать `ClawConfig` → `map[string]string` Metadata, передавать env vars через SpawnRequest

---

## Phase 2: PicoClaw — Proto и gRPC зависимости

### 2.1 Копия proto в PicoClaw

Копируем `F:/AI/Claude/HiveKernel/api/proto/agent.proto` → `F:/AI/Claude/picoclaw/api/proto/hive/agent.proto`

Меняем `go_package`:
```protobuf
option go_package = "github.com/sipeed/picoclaw/pkg/hive/hivepb";
```

Генерируем Go код:
```bash
protoc --go_out=pkg/hive/hivepb --go-grpc_out=pkg/hive/hivepb agent.proto
```

### 2.2 go.mod зависимости

Добавить в PicoClaw:
```
google.golang.org/grpc
google.golang.org/protobuf
```

---

## Phase 3: PicoClaw — `hive` command

### 3.1 CLI entry point

**File**: `F:/AI/Claude/picoclaw/cmd/picoclaw/main.go`

Добавить case `"hive"` в switch, вызывающий `hiveCmd()`.

### 3.2 Новый файл `hive.go`

**New file**: `F:/AI/Claude/picoclaw/cmd/picoclaw/hive.go`

```
hiveCmd():
  1. Парсит флаги: --core, --port, --debug
  2. Читает env: HIVEKERNEL_PID, HIVEKERNEL_CORE
  3. net.Listen("tcp", ":0") — auto-select port
  4. Создаёт HiveAgentServer
  5. Регистрирует AgentService на gRPC server
  6. fmt.Printf("READY %d\n", port) — READY protocol
  7. grpcServer.Serve(lis)
  8. Ждёт SIGINT/SIGTERM → GracefulStop
```

---

## Phase 4: PicoClaw — пакет `pkg/hive/`

### 4.1 `server.go` — HiveAgentServer

**New file**: `F:/AI/Claude/picoclaw/pkg/hive/server.go`

Struct:
```go
type HiveAgentServer struct {
    pb.UnimplementedAgentServiceServer
    coreAddr   string
    pid        uint64
    ppid       uint64
    agentLoop  *agent.AgentLoop
    msgBus     *bus.MessageBus
    cfg        *config.Config
    provider   providers.LLMProvider
    mu         sync.Mutex
    cancelFunc context.CancelFunc  // для Interrupt
}
```

RPC реализации:

**Init()**:
1. Сохранить pid, ppid
2. Собрать `config.Config` из InitRequest.Config (model, metadata → workspace, provider, etc.)
3. Читать API keys из env (HiveKernel инжектит через cmd.Env)
4. `providers.CreateProvider(cfg)`
5. `agent.NewAgentLoop(cfg, msgBus, provider)`
6. Return `InitResponse{Ready: true}`

**Execute()** (bidi stream):
1. Recv первое сообщение → TaskRequest
2. Создать `SyscallBridge(stream, pid)`
3. Подменить tools: `agentLoop.RegisterTool(NewHiveSpawnTool(bridge))`
4. `agentLoop.ProcessDirect(ctx, task.Description, sessionKey)`
5. Send `TaskProgress{PROGRESS_COMPLETED, result}`
6. При ошибке — `PROGRESS_FAILED`

**Heartbeat()**: Return state (idle/running)

**DeliverMessage()**: Convert to `bus.InboundMessage`, publish to msgBus

**Shutdown()**: `agentLoop.Stop()`, `msgBus.Close()`, exit

**Interrupt()**: Cancel context через cancelFunc

### 4.2 `bridge.go` — SyscallBridge

**New file**: `F:/AI/Claude/picoclaw/pkg/hive/bridge.go`

SyscallBridge управляет syscall request/response через Execute stream:

```go
type SyscallBridge struct {
    stream    pb.AgentService_ExecuteServer
    pid       uint64
    callSeq   atomic.Uint64
    pending   map[string]chan *pb.SyscallResult
    mu        sync.Mutex
}
```

Проблема: Execute stream — единственный goroutine где агент отправляет/получает данные. Но `ProcessDirect` вызывается синхронно в том же goroutine. Решение:

- `Execute()` запускает `recvLoop` в отдельной goroutine, которая читает `stream.Recv()` и роутит SyscallResult в `pending[callID]` channel
- `Syscall()` метод: отправляет TaskProgress{PROGRESS_SYSCALL}, ждёт на channel
- `ProcessDirect` вызывает tools → tool вызывает `bridge.Syscall()` → блокируется до ответа от ядра

### 4.3 `tools.go` — Hive-bridged tools

**New file**: `F:/AI/Claude/picoclaw/pkg/hive/tools.go`

4 инструмента, реализующих `tools.Tool` interface:

| Tool | PicoClaw native | Hive-bridged |
|------|-----------------|--------------|
| `HiveSpawnTool` | spawn → SubagentManager goroutine | spawn → syscall → kernel creates new picoclaw process |
| `HiveSendTool` | message → bus.PublishOutbound | send → syscall → kernel IPC broker → target DeliverMessage |
| `HiveStoreTool` | write_file | store_artifact → syscall → kernel shared memory |
| `HiveWaitTool` | — | wait_child → syscall → kernel waits for child exit |

Каждый tool в `Execute()`:
1. Формирует `pb.SystemCall` из args
2. Вызывает `bridge.Syscall(call)`
3. Парсит `pb.SyscallResult`
4. Возвращает `tools.ToolResult`

Остальные picoclaw tools (read_file, write_file, exec, web_search, i2c, spi) работают без изменений — локально.

---

## Phase 5: Интеграция потоков

### 5.1 Startup: Kernel → PicoClaw boot

```
1. Kernel читает startup.json с runtime_type=RUNTIME_CLAW
2. spawnStartupAgents → king.SpawnChild(SpawnRequest{RuntimeType: "RUNTIME_CLAW", RuntimeImage: "picoclaw"})
3. king.SpawnChild → spawner.Spawn (регистрация в registry) → rtManager.StartRuntime(proc, RuntimeClaw)
4. manager.spawnClaw: exec.Command("picoclaw", "hive", "--core", coreAddr)
   - env: HIVEKERNEL_PID=N, HIVEKERNEL_CORE=localhost:50051, + ClawConfig.Env
5. picoclaw hive: listen tcp, print "READY <port>"
6. kernel: grpc.NewClient → Init(pid, ppid, role, config{name, model, metadata})
7. picoclaw: buildConfigFromInit → CreateProvider → NewAgentLoop → Ready
```

### 5.2 Task execution: Kernel → PicoClaw → syscalls → Kernel

```
1. Kernel executor: stream = client.Execute(ctx)
2. Kernel → stream.Send(TaskRequest{description})
3. picoclaw Execute():
   a. Recv TaskRequest
   b. Create SyscallBridge (starts recvLoop goroutine)
   c. Register hive tools (spawn, send, store, wait)
   d. agentLoop.ProcessDirect(ctx, description, sessionKey)
      → LLM → tool_calls → execute tools
      → если tool=HiveSpawnTool → bridge.Syscall(spawn) → stream.Send(PROGRESS_SYSCALL)
         → ядро recv → HandleSyscall → king.SpawnChild → новый picoclaw процесс
         → ядро → stream.Send(SyscallResult{child_pid})
         → bridge.recvLoop → pending[callID] <- result
         → HiveSpawnTool.Execute returns ToolResult{"spawned PID N"}
         → LLM получает результат, продолжает
      → если tool=exec/read_file/web_search → локальное выполнение (без syscall)
   e. ProcessDirect returns content
4. picoclaw → stream.Send(PROGRESS_COMPLETED{output: content})
5. Kernel executor → return TaskResult
```

### 5.3 Queen spawns worker

```
1. Queen (picoclaw, PID 2) получает задачу "Исследуй тему X"
2. LLM решает: нужен worker
3. LLM вызывает tool spawn(task="research topic X", name="researcher-1")
4. HiveSpawnTool → bridge.Syscall(SpawnRequest{
     name: "researcher-1", role: WORKER, runtime_type: RUNTIME_CLAW,
     runtime_image: "picoclaw", initial_task: "research topic X"
   })
5. Kernel: handleSpawn → king.SpawnChild(parentPID=2, ...)
   → spawner validates (worker tier >= parent tier: OK)
   → rtManager.spawnClaw → exec.Command("picoclaw", "hive", ...) → READY → Init
6. Kernel returns SpawnResponse{child_pid: 5}
7. Queen может вызвать execute_on(pid=5, task="research topic X")
   ИЛИ ядро автоматически отправляет initial_task
8. Worker-5 (picoclaw) выполняет задачу, возвращает результат
9. Queen вызывает wait_child(pid=5) → получает результат
```

### 5.4 Inter-agent messaging (Python ↔ Claw)

```
1. Claw agent (PID 3) вызывает HiveSendTool(to_pid=4, content="hello")
2. bridge.Syscall(SendMessageRequest{to_pid=4}) → kernel broker
3. broker.Route → deliver to PID 4's inbox
4. Если PID 4 — Python: kernel вызывает pythonClient.DeliverMessage()
   Если PID 4 — Claw: kernel вызывает clawClient.DeliverMessage()
5. Claw DeliverMessage → msgBus.PublishInbound → агент обработает при следующем ProcessDirect
```

---

## Phase 6: Startup Config Format

**New file**: `F:/AI/Claude/HiveKernel/configs/startup-claw.json`

```json
{
  "agents": [
    {
      "name": "queen",
      "role": "daemon",
      "cognitive_tier": "tactical",
      "model": "anthropic/claude-sonnet-4-20250514",
      "runtime_type": "RUNTIME_CLAW",
      "runtime_image": "picoclaw",
      "system_prompt": "You are the Queen agent. Decompose complex tasks, spawn workers, coordinate results.",
      "claw_config": {
        "workspace": "./workspaces/queen",
        "provider": "anthropic",
        "max_tool_iterations": 25,
        "restrict_to_workspace": false,
        "env": {
          "ANTHROPIC_API_KEY": "${ANTHROPIC_API_KEY}"
        }
      }
    },
    {
      "name": "assistant",
      "role": "daemon",
      "cognitive_tier": "tactical",
      "model": "anthropic/claude-sonnet-4-20250514",
      "runtime_type": "RUNTIME_CLAW",
      "runtime_image": "picoclaw",
      "system_prompt": "You are the Assistant. Handle user messages from Telegram and Discord.",
      "claw_config": {
        "workspace": "./workspaces/assistant",
        "provider": "anthropic",
        "channels": ["telegram", "discord"]
      }
    },
    {
      "name": "coder",
      "role": "daemon",
      "cognitive_tier": "tactical",
      "model": "sonnet",
      "runtime_type": "RUNTIME_PYTHON",
      "runtime_image": "hivekernel_sdk.coder:CoderAgent"
    }
  ]
}
```

---

## Порядок реализации

| # | Задача | Проект | Файлы | Статус |
|---|--------|--------|-------|--------|
| 1 | Добавить SystemPrompt, Metadata в Process + SpawnRequest | HiveKernel | `process/types.go`, `process/spawner.go` | DONE |
| 2 | ClawAgentConfig в startup.go + парсинг в metadata | HiveKernel | `kernel/startup.go` | DONE |
| 3 | Рефакторинг spawnReal -> spawnPython + launchAndConnect | HiveKernel | `runtime/manager.go` | DONE |
| 4 | Новый spawnClaw + resolveClawBinary | HiveKernel | `runtime/manager.go` | DONE |
| 5 | Обогатить InitRequest в launchAndConnect | HiveKernel | `runtime/manager.go` | DONE |
| 6 | --claw-bin флаг + ClawConfig->metadata в main.go | HiveKernel | `cmd/hivekernel/main.go` | DONE |
| 7 | Тесты HiveKernel (dispatch, config, init) | HiveKernel | `runtime/manager_test.go`, `kernel/startup_test.go` | DONE |
| 8 | Копия agent.proto + codegen | PicoClaw | `api/proto/hive/`, `pkg/hive/hivepb/` | DONE |
| 9 | go.mod: grpc + protobuf зависимости | PicoClaw | `go.mod` | DONE |
| 10 | pkg/hive/server.go -- HiveAgentServer | PicoClaw | `pkg/hive/server.go` | DONE |
| 11 | pkg/hive/bridge.go -- SyscallBridge | PicoClaw | `pkg/hive/bridge.go` | DONE |
| 12 | pkg/hive/tools.go -- 4 Hive-bridged tools | PicoClaw | `pkg/hive/tools.go` | DONE |
| 13 | cmd/picoclaw/hive.go -- CLI command | PicoClaw | `cmd/picoclaw/hive.go` | DONE |
| 14 | Добавить case "hive" в main.go switch | PicoClaw | `cmd/picoclaw/main.go` | DONE |
| 15 | Тесты PicoClaw (server, bridge, tools) | PicoClaw | `pkg/hive/*_test.go` | DONE |
| 16 | Integration test config | HiveKernel | `configs/startup-claw.json` | DONE |
| 17 | E2E test (kernel + picoclaw boot + task + syscall) | Both | `tests/` | TODO |

**Реализовано 16/17 шагов. Осталось: E2E test.**

---

## Что реализовано (2026-02-16)

### HiveKernel

**process/types.go**: +`SystemPrompt string`, +`Metadata map[string]string` в `Process` struct.

**process/spawner.go**: +`Metadata map[string]string` в `SpawnRequest`, копируются в `Spawn()`.

**kernel/startup.go**: +`ClawAgentConfig` struct (Workspace, Provider, MaxToolIterations, RestrictToWorkspace, Channels, Env), +`ClawConfigToMetadata()` конвертер, +`ClawConfig *ClawAgentConfig` поле в `StartupAgent`.

**runtime/manager.go**: Полный рефакторинг:
- `spawnReal` теперь dispatcher по `RuntimeType` (Python/Claw/unsupported)
- `spawnPython` -- бывшее тело spawnReal, формирует `exec.Command` для Python runner
- `spawnClaw` -- формирует `exec.Command("picoclaw", "hive", "--core", coreAddr)`, инжектит env из metadata
- `launchAndConnect` -- общий код: stdout pipe, READY protocol, gRPC dial, enriched Init(PID, PPID, User, Role, CognitiveTier, Config{Name, Model, SystemPrompt, Metadata}, Limits)
- +`clawBin` field, `SetClawBin()`, `resolveClawBinary()`
- +`cogToProto()`, `limitsToProto()` helpers

**cmd/hivekernel/main.go**: +`--claw-bin` flag, `ClawConfigToMetadata()` вызов в `spawnStartupAgents`.

**configs/startup-claw.json**: пример конфига с queen (CLAW), assistant (CLAW), coder (Python).

**Тесты**: +10 тестов в manager_test.go (SetClawBin, ResolveClawBinary x4, SpawnReal_Unsupported, SpawnReal_NoImage, CogToProto, LimitsToProto), +4 тестов в startup_test.go (ClawConfigToMetadata x3, LoadStartupConfig_WithClawConfig). Все 8 пакетов проходят.

### PicoClaw

**api/proto/hive/agent.proto**: копия HiveKernel agent.proto с `go_package = "github.com/sipeed/picoclaw/pkg/hive/hivepb"`.

**pkg/hive/hivepb/**: сгенерированный protoc Go код (agent.pb.go, agent_grpc.pb.go).

**pkg/hive/server.go** (~200 LOC): `HiveAgentServer` implements `AgentServiceServer`:
- `Init()`: buildConfigFromInit -> CreateProvider -> NewAgentLoop
- `Execute()`: recv TaskRequest -> SyscallBridge + register hive tools -> ProcessDirect -> send COMPLETED/FAILED
- `Heartbeat()`: returns IDLE/RUNNING
- `DeliverMessage()`: converts to bus.InboundMessage
- `Shutdown()`: cancel + agentLoop.Stop()
- `Interrupt()`: cancel context
- `buildConfigFromInit()`: parses model, claw.* metadata into config.Config

**pkg/hive/bridge.go** (~130 LOC): `SyscallBridge`:
- `recvLoop` goroutine reads stream, routes SyscallResult to pending channels
- `Syscall()`: assigns call_id, sends PROGRESS_SYSCALL, blocks on channel
- `SendProgress()`: non-syscall progress messages
- `Close()`: wakes pending callers

**pkg/hive/tools.go** (~250 LOC): 4 tools implementing `tools.Tool`:
- `HiveSpawnTool` (hive_spawn): spawn child process via syscall
- `HiveSendTool` (hive_send): send IPC message via syscall
- `HiveStoreTool` (hive_store): store artifact via syscall
- `HiveWaitTool` (hive_wait): waitpid via syscall

**cmd/picoclaw/hive.go** (~80 LOC): `hiveCmd()`:
- Parses --core, --port, --debug flags + HIVEKERNEL_CORE env
- net.Listen tcp :0 (auto-port)
- Creates HiveAgentServer, registers on gRPC, prints "READY <port>"
- Waits SIGINT/SIGTERM -> GracefulStop

**cmd/picoclaw/main.go**: +case "hive" в switch, +hive в help.

**go.mod**: +google.golang.org/grpc v1.79.1, +google.golang.org/protobuf v1.36.11.

**Тесты**: 27 тестов в pkg/hive/ (server_test, bridge_test, tools_test). Все проходят.

---

## Verification

### Unit tests
```bash
# HiveKernel -- all 8 packages pass
cd F:/AI/Claude/HiveKernel
"C:\Program Files\Go\bin\go.exe" test ./internal/... -v

# PicoClaw hive -- 27 tests pass
cd F:/AI/Claude/picoclaw
"C:\Program Files\Go\bin\go.exe" test ./pkg/hive/... -v
```

### Build
```bash
cd F:/AI/Claude/HiveKernel && "C:\Program Files\Go\bin\go.exe" build -o bin/hivekernel.exe ./cmd/hivekernel
cd F:/AI/Claude/picoclaw && "C:\Program Files\Go\bin\go.exe" build -o bin/picoclaw.exe ./cmd/picoclaw
```

### Integration test (manual, requires API key)
```bash
bin/hivekernel.exe --listen :50051 --startup configs/startup-claw.json --claw-bin F:/AI/Claude/picoclaw/bin/picoclaw.exe
```

### Smoke test checklist
- [x] Kernel starts, reads startup-claw.json (config parses correctly)
- [ ] PicoClaw process spawns, prints READY, Init succeeds (needs API key)
- [ ] Heartbeat returns STATE_IDLE
- [ ] Execute with simple task returns PROGRESS_COMPLETED
- [ ] Spawn syscall from claw agent creates child process
- [ ] Send syscall delivers message to another agent (Python or Claw)
- [ ] Shutdown RPC gracefully stops picoclaw
- [ ] Mixed tree: Python + Claw agents communicate via IPC broker

## TODO

- [ ] E2E test script (step 17)
- [x] SystemPrompt override in PicoClaw ContextBuilder -- DONE (config.go + context.go + server.go + loop.go)
- [x] Workspace bootstrap for hive mode (IDENTITY.md, memory/, sessions/) -- DONE (server.go bootstrapHiveWorkspace)
- [x] API key error hint in hive mode -- DONE (server.go Init)
