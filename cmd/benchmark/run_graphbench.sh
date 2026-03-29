#!/usr/bin/env bash
# run_graphbench.sh — OOM-safe batch runner for GraphBench.
#
# Processes repos one at a time (configurable), restarting the daemon between
# batches to prevent memory accumulation. Combines per-batch results at the end.
#
# Usage:
#   RESULTS_DIR=results/graphbench ./cmd/benchmark/run_graphbench.sh [--batch-size N]
#
# Environment:
#   RESULTS_DIR    — where to write results (default: results/graphbench)
#   REPOS_DIR      — where repos are cloned (default: /tmp/graphbench_repos)
#   DAEMON_BIN     — path to synapses binary (default: auto-detect from go build)
#   BENCHMARK_BIN  — path to benchmark binary (default: auto-detect from go build)
#   ENDPOINT       — daemon HTTP endpoint (default: http://127.0.0.1:11435)

set -euo pipefail

# ─── Configuration ───────────────────────────────────────────────────────────

BATCH_SIZE=1
RESULTS_DIR="${RESULTS_DIR:-results/graphbench}"
REPOS_DIR="${REPOS_DIR:-/tmp/graphbench_repos}"
ENDPOINT="${ENDPOINT:-http://127.0.0.1:11435}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
DATA_FILE="$SCRIPT_DIR/graphbench.jsonl"

# Parse args.
while [[ $# -gt 0 ]]; do
    case "$1" in
        --batch-size) BATCH_SIZE="$2"; shift 2 ;;
        --batch-size=*) BATCH_SIZE="${1#*=}"; shift ;;
        *) echo "Unknown arg: $1"; exit 1 ;;
    esac
done

# ─── Build ───────────────────────────────────────────────────────────────────

echo "=== Building binaries ==="
cd "$PROJECT_ROOT"

DAEMON_BIN="${DAEMON_BIN:-}"
BENCHMARK_BIN="${BENCHMARK_BIN:-}"

if [[ -z "$DAEMON_BIN" ]]; then
    go build -o /tmp/synapses_bench ./cmd/synapses/...
    DAEMON_BIN="/tmp/synapses_bench"
fi

if [[ -z "$BENCHMARK_BIN" ]]; then
    go build -o /tmp/benchmark_bench ./cmd/benchmark/...
    BENCHMARK_BIN="/tmp/benchmark_bench"
fi

mkdir -p "$RESULTS_DIR" "$REPOS_DIR"

# ─── Helpers ─────────────────────────────────────────────────────────────────

DAEMON_PID=""

start_daemon() {
    echo "Starting daemon..."
    "$DAEMON_BIN" daemon &
    DAEMON_PID=$!
    # Wait for daemon to be ready.
    for i in $(seq 1 30); do
        if curl -s "$ENDPOINT/health" >/dev/null 2>&1; then
            echo "Daemon ready (PID=$DAEMON_PID)"
            return 0
        fi
        sleep 1
    done
    echo "WARNING: Daemon may not be ready after 30s"
}

stop_daemon() {
    if [[ -n "$DAEMON_PID" ]] && kill -0 "$DAEMON_PID" 2>/dev/null; then
        echo "Stopping daemon (PID=$DAEMON_PID)..."
        kill "$DAEMON_PID" 2>/dev/null || true
        wait "$DAEMON_PID" 2>/dev/null || true
        DAEMON_PID=""
        sleep 2  # Let OS reclaim memory.
    fi
}

check_memory() {
    if command -v vm_stat >/dev/null 2>&1; then
        # macOS: check available memory via vm_stat.
        local free_pages
        free_pages=$(vm_stat | awk '/Pages free/ {gsub(/\./,"",$3); print $3}')
        local free_mb=$((free_pages * 4096 / 1024 / 1024))
        if [[ $free_mb -lt 1024 ]]; then
            echo "WARNING: Low memory (${free_mb}MB free). Restarting daemon."
            stop_daemon
            sleep 5
            start_daemon
        fi
    fi
}

cleanup() {
    stop_daemon
    echo "Cleanup complete."
}

trap cleanup EXIT INT TERM

# ─── Count total suites ──────────────────────────────────────────────────────

TOTAL_SUITES=$(grep -c '^{' "$DATA_FILE" || echo 0)
echo "=== GraphBench: $TOTAL_SUITES repo suites, batch size $BATCH_SIZE ==="

# ─── Run in batches ──────────────────────────────────────────────────────────

BATCH_NUM=0
OFFSET=0

start_daemon

while [[ $OFFSET -lt $TOTAL_SUITES ]]; do
    BATCH_NUM=$((BATCH_NUM + 1))
    LIMIT=$BATCH_SIZE
    REMAINING=$((TOTAL_SUITES - OFFSET))
    if [[ $LIMIT -gt $REMAINING ]]; then
        LIMIT=$REMAINING
    fi

    echo ""
    echo "=== Batch $BATCH_NUM: repos $((OFFSET+1))-$((OFFSET+LIMIT)) of $TOTAL_SUITES ==="

    check_memory

    # Create a temp JSONL file with just this batch's suites.
    BATCH_FILE="$RESULTS_DIR/batch_${BATCH_NUM}.jsonl"
    sed -n "$((OFFSET+1)),$((OFFSET+LIMIT))p" <(grep '^{' "$DATA_FILE") > "$BATCH_FILE"

    BATCH_RESULT="$RESULTS_DIR/batch_${BATCH_NUM}"
    mkdir -p "$BATCH_RESULT"

    "$BENCHMARK_BIN" \
        --benchmark=graphbench \
        --gb-data="$BATCH_FILE" \
        --repos-dir="$REPOS_DIR" \
        --output-dir="$BATCH_RESULT" \
        --endpoint="$ENDPOINT" \
        2>&1 | tee "$RESULTS_DIR/batch_${BATCH_NUM}.log"

    OFFSET=$((OFFSET + LIMIT))

    # Between batches: restart daemon to free memory.
    if [[ $OFFSET -lt $TOTAL_SUITES ]]; then
        echo "Restarting daemon between batches..."
        stop_daemon
        sleep 3
        start_daemon
    fi
done

stop_daemon

# ─── Combine results ─────────────────────────────────────────────────────────

echo ""
echo "=== Combining batch results ==="

# Find all batch JSON result files and combine them.
COMBINED="$RESULTS_DIR/graphbench_combined.json"
python3 -c "
import json, glob, sys

results = []
for f in sorted(glob.glob('$RESULTS_DIR/batch_*/graphbench_*.json')):
    with open(f) as fh:
        results.append(json.load(fh))

if not results:
    print('No batch results found', file=sys.stderr)
    sys.exit(1)

# Merge: concatenate tests, recompute metrics.
all_tests = []
for r in results:
    all_tests.extend(r.get('tests', []))

total = len(all_tests)
errors = sum(1 for t in all_tests if t.get('error'))
non_error = [t for t in all_tests if not t.get('error')]

correct = sum(1 for t in non_error if t.get('recall', 0) > 0)
complete = sum(1 for t in non_error if t.get('recall', 0) >= 1.0)

n = len(non_error) or 1
avg_p = sum(t.get('precision', 0) for t in non_error) / n
avg_r = sum(t.get('recall', 0) for t in non_error) / n
avg_f1 = sum(t.get('f1', 0) for t in non_error) / n

combined = {
    'timestamp': results[-1]['timestamp'],
    'total_tests': total,
    'error_count': errors,
    'correctness': correct / n if n else 0,
    'completeness': complete / n if n else 0,
    'summary': {'precision': avg_p, 'recall': avg_r, 'f1': avg_f1},
    'tests': all_tests,
}

with open('$COMBINED', 'w') as fh:
    json.dump(combined, fh, indent=2)

print(f'Combined: {total} tests, {errors} errors')
print(f'Correctness: {correct}/{n} = {correct/n*100:.1f}%')
print(f'Completeness: {complete}/{n} = {complete/n*100:.1f}%')
print(f'P={avg_p*100:.1f}% R={avg_r*100:.1f}% F1={avg_f1*100:.1f}%')
"

echo ""
echo "=== Done ==="
echo "Combined results: $COMBINED"
echo "Batch logs: $RESULTS_DIR/batch_*.log"
