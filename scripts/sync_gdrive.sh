#!/usr/bin/env bash
# ==============================================================================
# LyricsPlus - Google Drive to Local SQLite Migration Script
# ==============================================================================
set -e

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$DIR/gdrive_sync"
PID_FILE="$DIR/data/.gdrive_sync.pid"
LOG_FILE="$DIR/data/gdrive_sync.log"

CONCURRENCY=500
RESET_FLAG=""
CONFIG_FLAG=""
RUN_BG=false
CHECK_STATUS=false
STOP_RUN=false

mkdir -p "$DIR/data" "$DIR/database"

# Ensure binary is compiled
if [ ! -f "$BIN" ]; then
    echo "[*] Compiling gdrive_sync binary..."
    (cd "$DIR" && go build -o gdrive_sync ./cmd/gdrive_sync)
fi

# Parse CLI arguments
while [[ $# -gt 0 ]]; do
    case "$1" in
        --background|-bg)
            RUN_BG=true
            shift
            ;;
        --status)
            CHECK_STATUS=true
            shift
            ;;
        --stop)
            STOP_RUN=true
            shift
            ;;
        --reset)
            RESET_FLAG="-reset"
            shift
            ;;
        --config)
            CONFIG_FLAG="-config $2"
            shift 2
            ;;
        --concurrency|-c)
            CONCURRENCY="$2"
            shift 2
            ;;
        --help|-h)
            echo "Usage: $0 [options]"
            echo ""
            echo "Options:"
            echo "  (default)            Run interactive migration in foreground"
            echo "  --config <path>      Path to config file (.json or .env)"
            echo "  --background, -bg    Run migration in background (detaches, logs to data/gdrive_sync.log)"
            echo "  --status             Check status of background migration"
            echo "  --stop               Gracefully stop background migration"
            echo "  --reset              Start migration from scratch (ignore existing checkpoint)"
            echo "  --concurrency, -c N  Number of concurrent download workers (default: 500)"
            echo "  --help, -h           Show this help message"
            exit 0
            ;;
        *)
            echo "Unknown argument: $1"
            echo "Run '$0 --help' for usage."
            exit 1
            ;;
    esac
done

# Handle --status
if [ "$CHECK_STATUS" = true ]; then
    if [ -f "$PID_FILE" ]; then
        PID=$(cat "$PID_FILE")
        if ps -p "$PID" > /dev/null 2>&1; then
            echo "[+] gdrive_sync is RUNNING (PID: $PID)"
            echo ""
            if [ -f "$DIR/data/.gdrive_sync_checkpoint.json" ]; then
                echo "Checkpoint state:"
                cat "$DIR/data/.gdrive_sync_checkpoint.json"
                echo ""
            fi
            echo "Latest log lines (data/gdrive_sync.log):"
            tail -n 10 "$LOG_FILE" 2>/dev/null || true
            exit 0
        else
            echo "[-] Stale PID file found (process $PID not running)."
            rm -f "$PID_FILE"
        fi
    else
        echo "[-] gdrive_sync is NOT running."
    fi
    exit 0
fi

# Handle --stop
if [ "$STOP_RUN" = true ]; then
    if [ -f "$PID_FILE" ]; then
        PID=$(cat "$PID_FILE")
        if ps -p "$PID" > /dev/null 2>&1; then
            echo "[*] Sending graceful stop signal to PID $PID..."
            kill -INT "$PID"
            echo "[+] Stopped."
        else
            echo "[-] Process $PID not running."
        fi
        rm -f "$PID_FILE"
    else
        echo "[-] No running migration found."
    fi
    exit 0
fi

# Check if already running
if [ -f "$PID_FILE" ]; then
    OLD_PID=$(cat "$PID_FILE")
    if ps -p "$OLD_PID" > /dev/null 2>&1; then
        echo "[!] Migration is already running with PID: $OLD_PID"
        echo "Run '$0 --status' to view progress, or '$0 --stop' to halt it."
        exit 1
    fi
fi

# Run in background mode
if [ "$RUN_BG" = true ]; then
    echo "[+] Starting gdrive_sync in background (concurrency: $CONCURRENCY)..."
    nohup "$BIN" $CONFIG_FLAG -concurrency "$CONCURRENCY" $RESET_FLAG >> "$LOG_FILE" 2>&1 &
    PID=$!
    echo "$PID" > "$PID_FILE"
    echo "[+] Running with PID: $PID"
    echo "[+] Log file: $LOG_FILE"
    echo "Check progress with: $0 --status or tail -f $LOG_FILE"
    exit 0
fi

# Run in foreground mode
echo "[+] Starting gdrive_sync in foreground (concurrency: $CONCURRENCY)..."
echo "[*] Press Ctrl+C at any time to save checkpoint and exit."
echo ""
exec "$BIN" $CONFIG_FLAG -concurrency "$CONCURRENCY" $RESET_FLAG
