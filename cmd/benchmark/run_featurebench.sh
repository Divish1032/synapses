#!/usr/bin/env bash
# run_featurebench.sh — Sprint 30.8 FeatureBench pilot (10 tasks)
#
# Runs 10 FeatureBench tasks in baseline and synapses modes,
# then compares resolve rates. This is the Sprint 30 gate.
#
# Prerequisites:
#   - Docker Desktop running
#   - Claude CLI on PATH (claude -p)
#   - Synapses binary built (go build ./cmd/synapses)
#   - Python: pip install datasets swebench
#
# Usage:
#   ./run_featurebench.sh                    # full run (both modes)
#   ./run_featurebench.sh --mode=baseline    # baseline only
#   ./run_featurebench.sh --mode=synapses    # synapses only
#   ./run_featurebench.sh --no-eval          # skip Docker eval

set -euo pipefail

# Ensure node/npm/claude and go are on PATH.
export PATH="/opt/homebrew/bin:/Users/itachi/.npm-global/bin:/usr/local/bin:$PATH"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
RESULTS_DIR="${RESULTS_DIR:-$SCRIPT_DIR/results/featurebench_pilot}"
REPOS_DIR="${REPOS_DIR:-/tmp/featurebench_repos}"
DATA_FILE="$SCRIPT_DIR/featurebench_pilot.jsonl"
MODEL="${MODEL:-claude-sonnet-4-6}"
TIMEOUT="${TIMEOUT:-900}"  # 15 min per task (feature tasks are harder)

# Parse args
MODE="both"
EVAL="--tb-eval"
DEBUG=""
for arg in "$@"; do
    case "$arg" in
        --mode=*) MODE="${arg#*=}" ;;
        --no-eval) EVAL="--tb-eval=false" ;;
        --debug) DEBUG="--tb-debug" ;;
    esac
done

echo "╔══════════════════════════════════════════════════════════════╗"
echo "║  FeatureBench Pilot — Sprint 30.8 Gate                     ║"
echo "║  10 tasks × 2 modes (baseline + synapses)                  ║"
echo "║  Success: ≥+2pp delta OR actionable failure modes           ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""
echo "Config:"
echo "  Data:    $DATA_FILE"
echo "  Model:   $MODEL"
echo "  Mode:    $MODE"
echo "  Timeout: ${TIMEOUT}s per task"
echo "  Results: $RESULTS_DIR"
echo ""

# Verify prerequisites
echo "Checking prerequisites..."

if ! command -v docker &>/dev/null; then
    echo "ERROR: Docker not found. Start Docker Desktop first."
    exit 1
fi
if ! docker info &>/dev/null 2>&1; then
    echo "ERROR: Docker daemon not running. Start Docker Desktop."
    exit 1
fi
echo "  ✓ Docker running"

if ! command -v claude &>/dev/null; then
    echo "ERROR: claude CLI not found on PATH."
    echo "  Install: npm install -g @anthropic-ai/claude-code"
    exit 1
fi
echo "  ✓ Claude CLI available"

if ! python3 -c "import datasets" 2>/dev/null; then
    echo "ERROR: Python 'datasets' package missing."
    echo "  Install: pip install datasets"
    exit 1
fi
echo "  ✓ Python datasets"

if [ ! -f "$DATA_FILE" ]; then
    echo "ERROR: Task data not found at $DATA_FILE"
    echo "  Run: python3 $SCRIPT_DIR/scripts/load_bench_tasks.py --split fast > $DATA_FILE"
    exit 1
fi
TASK_COUNT=$(wc -l < "$DATA_FILE" | tr -d ' ')
echo "  ✓ $TASK_COUNT tasks loaded"

# Build benchmark binary
echo ""
echo "Building benchmark binary..."
cd "$REPO_ROOT"
go build -o "$SCRIPT_DIR/benchmark" ./cmd/benchmark
echo "  ✓ Built"

# Build synapses binary (needed for synapses mode)
if [ "$MODE" = "synapses" ] || [ "$MODE" = "both" ]; then
    echo "Building synapses binary..."
    go build -o "$SCRIPT_DIR/synapses" ./cmd/synapses
    export PATH="$SCRIPT_DIR:$PATH"
    echo "  ✓ Synapses built"
fi

mkdir -p "$RESULTS_DIR" "$REPOS_DIR"

echo ""
echo "Starting benchmark run..."
echo "  Start time: $(date '+%Y-%m-%d %H:%M:%S')"
echo ""

COMMON_ARGS=(
    --benchmark=taskbench
    --tb-data="$DATA_FILE"
    --tb-dataset="LiberCoders/FeatureBench"
    --tb-feature
    --repos-dir="$REPOS_DIR"
    --model="$MODEL"
    --tb-timeout="$TIMEOUT"
    --output-dir="$RESULTS_DIR"
    $EVAL
    $DEBUG
)

if [ "$MODE" = "both" ]; then
    "$SCRIPT_DIR/benchmark" "${COMMON_ARGS[@]}" --tb-both-modes
elif [ "$MODE" = "baseline" ] || [ "$MODE" = "synapses" ]; then
    "$SCRIPT_DIR/benchmark" "${COMMON_ARGS[@]}" --mode="$MODE"
else
    echo "ERROR: Unknown mode '$MODE'. Use: baseline | synapses | both"
    exit 1
fi

echo ""
echo "  End time: $(date '+%Y-%m-%d %H:%M:%S')"
echo ""
echo "Results written to: $RESULTS_DIR/"
echo ""

# Summary
echo "╔══════════════════════════════════════════════════════════════╗"
echo "║  Run complete. Check results in:                            ║"
echo "║  $RESULTS_DIR/                                              ║"
echo "║                                                             ║"
echo "║  Sprint 30.8 gate:                                         ║"
echo "║    ≥+2pp delta → proceed to Sprint 31                      ║"
echo "║    <+1pp, no insights → rethink architecture                ║"
echo "║    <+1pp, actionable modes → fix and re-run                 ║"
echo "╚══════════════════════════════════════════════════════════════╝"
