# Plan 006: Hybrid Hive Mode + Channels (Telegram)

**Status: COMPLETE** (2026-02-16)

## Goal

PicoClaw agents running in hive mode should also be able to listen on
messaging channels (Telegram, Discord, etc.).  This gives the user a
Telegram bot that is simultaneously a managed kernel agent -- it can
spawn sub-agents, use IPC, store artifacts, all through HiveKernel,
while the user simply chats via Telegram.

```
User (Telegram) --> PicoClaw (hive+channels) --> AgentLoop
                        |                           |
                        | gRPC (Execute)            | hive_spawn / hive_send
                        v                           v
                   HiveKernel core  <----------  child agents
                        |
                        v
                   Dashboard WebUI (monitoring)
```

## Architecture Overview

### Current state

| Mode | Channels | Kernel | Agent Loop |
|------|----------|--------|------------|
| `picoclaw gateway` | Telegram, Discord, ... | No | `Run()` -- event loop on bus |
| `picoclaw hive` | None | Yes (gRPC) | `ProcessDirect()` -- one-shot per task |

### Target state

| Mode | Channels | Kernel | Agent Loop |
|------|----------|--------|------------|
| `picoclaw hive` (with channels) | Telegram, ... | Yes (gRPC) | `Run()` in background + `ProcessDirect()` for kernel tasks |

### Key insight

- `Run()` blocks on `bus.ConsumeInbound()` and dispatches responses via
  `bus.PublishOutbound()` -- this is what channels need.
- `ProcessDirect()` calls `processMessage()` directly, bypassing the bus.
- Both call `processMessage()` which is sequential (LLM calls block), so
  messages are naturally serialized: a Telegram message waits while a
  kernel task is running, and vice versa.
- **Problem**: they'd compete. If `Run()` is consuming inbound and a
  `DeliverMessage` arrives (IPC), `Run()` picks it up. But `Execute()`
  calls `ProcessDirect()` directly. So no conflict -- `Run()` handles
  channel messages, `Execute()` handles kernel tasks.

## Changes

### Step 1: PicoClaw -- read channel tokens from env in buildConfigFromInit

**File**: `picoclaw/pkg/hive/server.go` -- `buildConfigFromInit()`

After reading provider API keys from env, also read channel tokens:

```go
// Channel tokens from env vars (injected by HiveKernel via claw_config.env).
if token := os.Getenv("PICOCLAW_CHANNELS_TELEGRAM_TOKEN"); token != "" {
    cfg.Channels.Telegram.Token = token
    cfg.Channels.Telegram.Enabled = true
}
if token := os.Getenv("PICOCLAW_CHANNELS_DISCORD_TOKEN"); token != "" {
    cfg.Channels.Discord.Token = token
    cfg.Channels.Discord.Enabled = true
}
```

Use standard PicoClaw env var names (`PICOCLAW_CHANNELS_TELEGRAM_TOKEN`)
so env tag parsing in config also works.  Also respect `claw.channels`
metadata to enable channels explicitly.

**~15 lines.**

### Step 2: PicoClaw -- add channel manager to HiveAgentServer

**File**: `picoclaw/pkg/hive/server.go`

Add field to struct:

```go
channelManager *channels.Manager
channelCtx     context.Context
channelCancel  context.CancelFunc
```

In `Init()`, after creating agentLoop (line ~96):

```go
// Start channel manager if any channels are enabled.
if hasEnabledChannels(cfg) {
    cm, err := channels.NewManager(cfg, s.msgBus)
    if err != nil {
        log.Printf("[hive] WARNING: channel manager: %v", err)
    } else {
        s.channelManager = cm
        s.agentLoop.SetChannelManager(cm)

        s.channelCtx, s.channelCancel = context.WithCancel(context.Background())
        if err := cm.StartAll(s.channelCtx); err != nil {
            log.Printf("[hive] WARNING: starting channels: %v", err)
        }

        // Run agent loop in background for channel messages.
        go s.agentLoop.Run(s.channelCtx)

        log.Printf("[hive] PID %d channels started", s.pid)
    }
}
```

Helper:

```go
func hasEnabledChannels(cfg *config.Config) bool {
    return cfg.Channels.Telegram.Enabled ||
        cfg.Channels.Discord.Enabled ||
        cfg.Channels.Slack.Enabled
}
```

**~30 lines.**

### Step 3: PicoClaw -- clean shutdown of channels

**File**: `picoclaw/pkg/hive/server.go` -- `Shutdown()`

Before closing shutdownCh:

```go
if s.channelCancel != nil {
    s.channelCancel()
}
if s.channelManager != nil {
    s.channelManager.StopAll(context.Background())
}
```

**~5 lines.**

### Step 4: HiveKernel startup config for Telegram demo

**File**: `HiveKernel/configs/startup-telegram.json`

```json
{
  "agents": [
    {
      "name": "assistant",
      "role": "agent",
      "cognitive_tier": "tactical",
      "model": "google/gemini-2.0-flash-001",
      "runtime_type": "RUNTIME_CLAW",
      "runtime_image": "picoclaw",
      "system_prompt": "You are a helpful assistant connected via Telegram. You can spawn sub-agents using hive_spawn tool for complex tasks. Keep responses concise.",
      "claw_config": {
        "workspace": "./workspaces/assistant",
        "provider": "openrouter",
        "max_tool_iterations": 10,
        "restrict_to_workspace": true,
        "channels": ["telegram"],
        "env": {
          "OPENROUTER_API_KEY": "${OPENROUTER_API_KEY}",
          "PICOCLAW_CHANNELS_TELEGRAM_TOKEN": "${TELEGRAM_BOT_TOKEN}"
        }
      }
    }
  ]
}
```

Cognitive tier = tactical so it can spawn operational children.

### Step 5: Update .env.example

**File**: `HiveKernel/.env.example`

Add:
```
# Telegram bot (for hybrid hive+channels mode)
# TELEGRAM_BOT_TOKEN=123456:ABC-DEF...
```

### Step 6: Telegram allow_from support

In `buildConfigFromInit`, also read allowlist:

```go
if allow := os.Getenv("PICOCLAW_CHANNELS_TELEGRAM_ALLOW_FROM"); allow != "" {
    cfg.Channels.Telegram.AllowFrom = strings.Split(allow, ",")
}
```

This lets the startup config restrict which Telegram users can talk to
the bot.

**~5 lines.**

## Concurrency notes

- `agentLoop.Run()` and `Execute()` both call `processMessage()`.
- `processMessage()` calls `runAgentLoop()` which calls `provider.Chat()`
  -- this is a blocking HTTP call, naturally serializing requests.
- If Execute and a Telegram message arrive simultaneously:
  - Execute calls ProcessDirect directly (not via bus)
  - Telegram message sits in the inbound bus channel (buffered, 100 capacity)
  - When ProcessDirect finishes, Run loop picks up the Telegram message
- This is fine for MVP.  For high-concurrency use, we'd need a mutex
  or routing Execute through the bus too.

## Files to create/modify

| File | Action | Lines |
|------|--------|-------|
| `picoclaw/pkg/hive/server.go` | Add channel support to Init/Shutdown/buildConfig | ~55 |
| `HiveKernel/configs/startup-telegram.json` | New startup config | ~25 |
| `HiveKernel/.env.example` | Add TELEGRAM_BOT_TOKEN | ~2 |

## Verification

```bash
# 1. Build both
cd F:/AI/Claude/HiveKernel && go build -o bin/hivekernel.exe ./cmd/hivekernel
cd F:/AI/Claude/picoclaw && go build -o bin/picoclaw.exe ./cmd/picoclaw

# 2. Existing tests still pass
cd F:/AI/Claude/HiveKernel && go test ./internal/... -v
cd F:/AI/Claude/picoclaw && go test ./pkg/hive/... -v

# 3. Set tokens in .env
#   OPENROUTER_API_KEY=sk-or-...
#   TELEGRAM_BOT_TOKEN=123456:ABC-DEF...

# 4. Run with Telegram
cd F:/AI/Claude/HiveKernel
bin/hivekernel.exe --startup configs/startup-telegram.json \
    --claw-bin F:/AI/Claude/picoclaw/bin/picoclaw.exe

# Expected logs:
#   [startup] loaded 2 var(s) from .env
#   [runtime] PID 2 (assistant) initialized successfully
#   [hive] PID 2 channels started
#
# Then message the bot on Telegram -- it should respond.

# 5. Also works with send_task:
go run demo/send_task/main.go "Hello from kernel"
# The agent processes it while Telegram is also active.
```

## Future extensions

- **Multi-agent from Telegram**: tactical-tier agent spawns children via
  hive_spawn, each child does subtask, results collected back.
- **Dashboard monitoring**: run `python dashboard/app.py` alongside --
  see the process tree grow in real-time as agents are spawned.
- **Telegram allow_from** via env var for security.
- **Voice messages**: PicoClaw already supports Groq voice transcription
  in Telegram channel -- works out of the box if Groq key is provided.
