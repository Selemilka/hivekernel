#!/usr/bin/env bash
# run_demo.sh -- Full HiveKernel + PicoClaw demo
#
# Prerequisites:
#   - OPENROUTER_API_KEY in .env file or environment
#   - Go installed
#
# Usage:
#   bash demo/run_demo.sh [question]
#
# Example:
#   bash demo/run_demo.sh "What is the capital of France?"

set -euo pipefail

HIVE_DIR="$(cd "$(dirname "$0")/.." && pwd)"
CLAW_DIR="$(cd "$HIVE_DIR/../picoclaw" && pwd)"
GO="go"
QUESTION="${1:-What is 2+2?}"

echo "=== HiveKernel + PicoClaw Demo ==="
echo ""

# Load .env if present (HiveKernel also loads it, but we check the key here).
if [ -f "$HIVE_DIR/.env" ]; then
    set -a
    source "$HIVE_DIR/.env"
    set +a
fi

# Check API key.
if [ -z "${OPENROUTER_API_KEY:-}" ]; then
    echo "ERROR: OPENROUTER_API_KEY is not set."
    echo "  Create $HIVE_DIR/.env with: OPENROUTER_API_KEY=sk-or-..."
    exit 1
fi

# Step 1: Build HiveKernel.
echo "[1/5] Building HiveKernel..."
(cd "$HIVE_DIR" && $GO build -o bin/hivekernel.exe ./cmd/hivekernel)
echo "  -> $HIVE_DIR/bin/hivekernel.exe"

# Step 2: Build PicoClaw.
echo "[2/5] Building PicoClaw..."
(cd "$CLAW_DIR" && $GO build -o bin/picoclaw.exe ./cmd/picoclaw)
echo "  -> $CLAW_DIR/bin/picoclaw.exe"

# Step 3: Start HiveKernel.
echo "[3/5] Starting HiveKernel..."
HIVE_LOG=$(mktemp)
"$HIVE_DIR/bin/hivekernel.exe" \
    --startup "$HIVE_DIR/configs/startup-demo.json" \
    --claw-bin "$CLAW_DIR/bin/picoclaw.exe" \
    > "$HIVE_LOG" 2>&1 &
HIVE_PID=$!
echo "  -> HiveKernel PID: $HIVE_PID (log: $HIVE_LOG)"

# Cleanup on exit.
cleanup() {
    echo ""
    echo "[5/5] Shutting down HiveKernel (PID $HIVE_PID)..."
    kill "$HIVE_PID" 2>/dev/null || true
    wait "$HIVE_PID" 2>/dev/null || true
    echo "  -> Done."
    echo ""
    echo "=== Full log ==="
    cat "$HIVE_LOG"
    rm -f "$HIVE_LOG"
}
trap cleanup EXIT

# Step 4: Wait for agent to be ready.
echo "[4/5] Waiting for agent to initialize..."
TIMEOUT=30
ELAPSED=0
while [ $ELAPSED -lt $TIMEOUT ]; do
    if grep -q "initialized successfully" "$HIVE_LOG" 2>/dev/null; then
        echo "  -> Agent ready!"
        break
    fi
    if grep -q "failed to spawn\|Init not ready\|provider error" "$HIVE_LOG" 2>/dev/null; then
        echo "  -> ERROR: Agent failed to start. Log:"
        cat "$HIVE_LOG"
        exit 1
    fi
    if ! kill -0 "$HIVE_PID" 2>/dev/null; then
        echo "  -> ERROR: HiveKernel exited unexpectedly. Log:"
        cat "$HIVE_LOG"
        exit 1
    fi
    sleep 1
    ELAPSED=$((ELAPSED + 1))
done

if [ $ELAPSED -ge $TIMEOUT ]; then
    echo "  -> TIMEOUT: Agent did not initialize within ${TIMEOUT}s. Log:"
    cat "$HIVE_LOG"
    exit 1
fi

# Small extra delay for gRPC to be fully ready.
sleep 1

# Step 5: Send task.
echo ""
echo "=== Sending task: $QUESTION ==="
echo ""
(cd "$HIVE_DIR" && $GO run demo/send_task/main.go "$QUESTION")
