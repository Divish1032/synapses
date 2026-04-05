#!/usr/bin/env bash
# agent_loop_test.sh — Fast agent-in-loop test for SynapsesBench.
#
# Runs Claude Code with a SHORT prompt (30s per task) in two modes:
# - Baseline: only Read/Grep/Glob tools
# - Synapses: Synapses MCP tools available
#
# Measures: does Synapses mode discover MORE relevant entities than baseline?
# Not a full coding task — just "what would you need to know before changing X?"
#
# Usage:
#   ./agent_loop_test.sh /path/to/indexed/repo "FunctionName"

set -euo pipefail
export PATH="/opt/homebrew/bin:/Users/itachi/.npm-global/bin:/usr/local/bin:$PATH"

REPO_DIR="${1:?Usage: $0 <repo_dir> <function_name>}"
FUNC_NAME="${2:?Usage: $0 <repo_dir> <function_name>}"
MODEL="${MODEL:-claude-sonnet-4-6}"
TIMEOUT=60
SYNAPSES_BIN="${SYNAPSES_BIN:-$HOME/.synapses/bin/synapses}"

# macOS doesn't have `timeout` — use a background process with kill
run_with_timeout() {
  local secs=$1
  shift
  "$@" &
  local pid=$!
  ( sleep "$secs" && kill "$pid" 2>/dev/null ) &
  local timer_pid=$!
  wait "$pid" 2>/dev/null
  local status=$?
  kill "$timer_pid" 2>/dev/null
  wait "$timer_pid" 2>/dev/null
  return $status
}

PROMPT="You are investigating the function '$FUNC_NAME' in this codebase. Your task: list ALL files and functions that would be affected if this function's signature changed. Just output a numbered list of file:function pairs. Do NOT modify any files. Be thorough — check callers, implementors, and transitive dependencies."

echo "═══════════════════════════════════════"
echo "  Agent-in-Loop Test: $FUNC_NAME"
echo "  Repo: $(basename $REPO_DIR)"
echo "═══════════════════════════════════════"
echo ""

# ── Baseline mode ────────────────────────────────────
echo "--- BASELINE (Read/Grep/Glob only) ---"
BASELINE_OUT=$(mktemp)
cd "$REPO_DIR"
claude -p "$PROMPT" \
  --allowedTools "Read Grep Glob Bash" \
  --model "$MODEL" \
  --max-turns 5 \
  --output-format stream-json \
  --verbose \
  < /dev/null > "$BASELINE_OUT" 2>/dev/null || true
cd - > /dev/null

BASELINE_RESULT=$(grep '"type":"result"' "$BASELINE_OUT" | tail -1 | python3 -c "import json,sys; d=json.loads(sys.stdin.read()); print(d.get('result',''))" 2>/dev/null || echo "")
BASELINE_TURNS=$(grep '"type":"result"' "$BASELINE_OUT" | tail -1 | python3 -c "import json,sys; d=json.loads(sys.stdin.read()); print(d.get('num_turns',0))" 2>/dev/null || echo "0")
BASELINE_COST=$(grep '"type":"result"' "$BASELINE_OUT" | tail -1 | python3 -c "import json,sys; d=json.loads(sys.stdin.read()); print(f'{d.get(\"total_cost_usd\",0):.4f}')" 2>/dev/null || echo "0")
BASELINE_ENTITIES=$(echo "$BASELINE_RESULT" | grep -c "^[0-9]" 2>/dev/null || echo 0)
BASELINE_ENTITIES=${BASELINE_ENTITIES##*$'\n'} # take last line only

echo "  Turns: $BASELINE_TURNS, Cost: \$$BASELINE_COST, Entities found: $BASELINE_ENTITIES"

# ── Synapses mode ────────────────────────────────────
echo ""
echo "--- SYNAPSES (MCP tools available) ---"

# Set up MCP config
mkdir -p "$REPO_DIR/.claude"
cat > "$REPO_DIR/.mcp.json" << MCPEOF
{
  "mcpServers": {
    "synapses": {
      "type": "stdio",
      "command": "$SYNAPSES_BIN",
      "args": ["start", "--path", "$REPO_DIR"]
    }
  }
}
MCPEOF
cat > "$REPO_DIR/.claude/settings.json" << SETEOF
{
  "permissions": {
    "allow": ["mcp__synapses__*", "Read(*)", "Grep(*)", "Glob(*)", "Bash(*)"]
  }
}
SETEOF

SYNAPSES_PROMPT="$PROMPT

IMPORTANT: You have Synapses MCP tools. Call mcp__synapses__get_impact(symbol=\"$FUNC_NAME\") to find affected entities. Then call mcp__synapses__search(query=\"$FUNC_NAME\") for additional context."

SYNAPSES_OUT=$(mktemp)
cd "$REPO_DIR"
claude -p "$SYNAPSES_PROMPT" \
  --allowedTools "Read Grep Glob Bash mcp__synapses__session_init mcp__synapses__search mcp__synapses__get_context mcp__synapses__get_impact mcp__synapses__validate" \
  --model "$MODEL" \
  --max-turns 5 \
  --output-format stream-json \
  --verbose \
  < /dev/null > "$SYNAPSES_OUT" 2>/dev/null || true

SYNAPSES_RESULT=$(grep '"type":"result"' "$SYNAPSES_OUT" | tail -1 | python3 -c "import json,sys; d=json.loads(sys.stdin.read()); print(d.get('result',''))" 2>/dev/null || echo "")
SYNAPSES_TURNS=$(grep '"type":"result"' "$SYNAPSES_OUT" | tail -1 | python3 -c "import json,sys; d=json.loads(sys.stdin.read()); print(d.get('num_turns',0))" 2>/dev/null || echo "0")
SYNAPSES_COST=$(grep '"type":"result"' "$SYNAPSES_OUT" | tail -1 | python3 -c "import json,sys; d=json.loads(sys.stdin.read()); print(f'{d.get(\"total_cost_usd\",0):.4f}')" 2>/dev/null || echo "0")
SYNAPSES_ENTITIES=$(echo "$SYNAPSES_RESULT" | grep -c "^[0-9]" 2>/dev/null || echo 0)
SYNAPSES_ENTITIES=${SYNAPSES_ENTITIES##*$'\n'}

# Count MCP tool calls
MCP_CALLS=$(grep -c "mcp__synapses" "$SYNAPSES_OUT" 2>/dev/null || echo "0")

echo "  Turns: $SYNAPSES_TURNS, Cost: \$$SYNAPSES_COST, Entities found: $SYNAPSES_ENTITIES, MCP calls: $MCP_CALLS"

# ── Comparison ────────────────────────────────────
echo ""
echo "═══════════════════════════════════════"
echo "  COMPARISON"
echo "═══════════════════════════════════════"
# Sanitize entity counts to integers
BASELINE_ENTITIES=$(echo "$BASELINE_ENTITIES" | tr -d '[:space:]')
SYNAPSES_ENTITIES=$(echo "$SYNAPSES_ENTITIES" | tr -d '[:space:]')
: "${BASELINE_ENTITIES:=0}"
: "${SYNAPSES_ENTITIES:=0}"

echo "  Entities: baseline=$BASELINE_ENTITIES synapses=$SYNAPSES_ENTITIES"

DELTA=$((SYNAPSES_ENTITIES - BASELINE_ENTITIES))
echo "  Delta: $DELTA additional entities with Synapses"
echo "  MCP calls: $MCP_CALLS"

if [ "$DELTA" -gt 0 ]; then
  echo "  RESULT: Synapses found MORE affected entities (+$DELTA)"
elif [ "$DELTA" -eq 0 ]; then
  echo "  RESULT: No difference"
else
  echo "  RESULT: Baseline found more (Synapses may have added noise)"
fi

# Cleanup
rm -f "$BASELINE_OUT" "$SYNAPSES_OUT"
rm -f "$REPO_DIR/.mcp.json"
rm -rf "$REPO_DIR/.claude"
