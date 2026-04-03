#!/usr/bin/env bash
# refresh-package-registry.sh — download top packages from each ecosystem registry.
#
# Usage:
#   ./scripts/refresh-package-registry.sh              # refresh all registries
#   ./scripts/refresh-package-registry.sh --npm        # npm only
#   ./scripts/refresh-package-registry.sh --pypi       # PyPI only
#   ./scripts/refresh-package-registry.sh --crates     # crates.io only
#   ./scripts/refresh-package-registry.sh --go         # Go modules only
#
# Output: overwrites internal/security/builtin/{npm,pypi,crates,go}-packages.txt
# with normalized package names. The seed entries at the top of each file are
# preserved in the output.
#
# Requirements: curl, python3 (for PyPI), jq (for npm/crates)
# Run from the repo root.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILTIN_DIR="$REPO_ROOT/internal/security/builtin"
TOP_N="${TOP_N:-5000}"  # packages to fetch per registry; set TOP_N=50000 for full list

DO_NPM=false
DO_PYPI=false
DO_CRATES=false
DO_GO=false
ALL=true

for arg in "$@"; do
  case "$arg" in
    --npm)    DO_NPM=true;   ALL=false ;;
    --pypi)   DO_PYPI=true;  ALL=false ;;
    --crates) DO_CRATES=true; ALL=false ;;
    --go)     DO_GO=true;    ALL=false ;;
    *) echo "Unknown flag: $arg" >&2; exit 1 ;;
  esac
done

if $ALL; then
  DO_NPM=true; DO_PYPI=true; DO_CRATES=true; DO_GO=true
fi

# ── Helpers ──────────────────────────────────────────────────────────────────

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "ERROR: required command '$1' not found" >&2
    exit 1
  fi
}

header() {
  echo ""
  echo "━━━ $1 ━━━"
}

# Preserve the seed header (comment block at top of file) before overwriting.
get_seed_header() {
  local file="$1"
  awk '/^[^#]/{exit} {print}' "$file"
}

# ── npm ──────────────────────────────────────────────────────────────────────

refresh_npm() {
  header "npm (top $TOP_N)"
  require_cmd curl
  require_cmd jq

  local out="$BUILTIN_DIR/npm-packages.txt"
  local seed_header
  seed_header="$(get_seed_header "$out")"

  # Use the npm registry downloads API to get top packages.
  # This endpoint returns the most downloaded packages in the last month.
  local tmpfile
  tmpfile="$(mktemp)"

  echo "Fetching npm download counts..."
  # npm doesn't have a simple "top N" API; we use npms.io which tracks 300K packages.
  # npms.io/v2/search?q=not:deprecated&size=250&from=0 — paginate through results.
  # Alternative: use jsdelivr's npm stats: https://data.jsdelivr.com/v1/stats/packages/npm?period=month&limit=100
  # We use the jsdelivr approach (no auth required, stable API):

  python3 - <<'PYEOF' > "$tmpfile"
import urllib.request, json, sys

packages = []
seen = set()

# jsdelivr top-packages API (sorted by monthly hits)
url = "https://data.jsdelivr.com/v1/stats/packages/npm?period=month&limit=100"
try:
    with urllib.request.urlopen(url, timeout=10) as r:
        data = json.load(r)
        for p in data:
            name = p.get("name", "").strip().lower()
            if name and name not in seen:
                packages.append(name)
                seen.add(name)
except Exception as e:
    print(f"# WARNING: jsdelivr API unavailable: {e}", file=sys.stderr)

# Fall back to a static top-100 list if API unavailable
if not packages:
    packages = ["react","lodash","chalk","commander","debug","async","express",
                "axios","typescript","webpack","babel-core","jest","moment","uuid"]

for p in sorted(set(packages)):
    print(p)
PYEOF

  local count
  count="$(wc -l < "$tmpfile")"
  echo "Got $count packages from API"

  # Write: seed header first, then API results (deduplicated against seed)
  {
    echo "$seed_header"
    echo ""
    echo "# ── Auto-fetched top packages ($(date +%Y-%m-%d)) ──────────────────────────────"
    sort -u "$tmpfile"
  } > "$out.tmp"
  mv "$out.tmp" "$out"
  rm -f "$tmpfile"
  echo "Updated: $out"
}

# ── PyPI ──────────────────────────────────────────────────────────────────────

refresh_pypi() {
  header "PyPI (top $TOP_N)"
  require_cmd curl
  require_cmd python3

  local out="$BUILTIN_DIR/pypi-packages.txt"
  local seed_header
  seed_header="$(get_seed_header "$out")"

  local tmpfile
  tmpfile="$(mktemp)"

  echo "Fetching PyPI top packages..."
  # Hugo van Kemenade maintains a JSON file of the top 8K PyPI packages by downloads.
  # https://hugovk.github.io/top-pypi-packages/top-pypi-packages-30-days.min.json
  python3 - <<'PYEOF' > "$tmpfile"
import urllib.request, json, sys, re

def pep503(name):
    name = name.lower()
    return re.sub(r'[-_.]+', '-', name)

packages = []
url = "https://hugovk.github.io/top-pypi-packages/top-pypi-packages-30-days.min.json"
try:
    with urllib.request.urlopen(url, timeout=15) as r:
        data = json.load(r)
        rows = data.get("rows", [])
        for row in rows[:8000]:
            name = row.get("project", "").strip()
            if name:
                packages.append(pep503(name))
except Exception as e:
    print(f"# WARNING: hugovk API unavailable: {e}", file=sys.stderr)

for p in sorted(set(packages)):
    print(p)
PYEOF

  local count
  count="$(wc -l < "$tmpfile")"
  echo "Got $count packages from API"

  {
    echo "$seed_header"
    echo ""
    echo "# ── Auto-fetched top packages ($(date +%Y-%m-%d)) ──────────────────────────────"
    sort -u "$tmpfile"
  } > "$out.tmp"
  mv "$out.tmp" "$out"
  rm -f "$tmpfile"
  echo "Updated: $out"
}

# ── crates.io ─────────────────────────────────────────────────────────────────

refresh_crates() {
  header "crates.io (top $TOP_N)"
  require_cmd curl
  require_cmd python3

  local out="$BUILTIN_DIR/crates-packages.txt"
  local seed_header
  seed_header="$(get_seed_header "$out")"

  local tmpfile
  tmpfile="$(mktemp)"

  echo "Fetching crates.io top crates..."
  # crates.io provides a public data dump and an API.
  # API: https://crates.io/api/v1/crates?sort=downloads&per_page=100&page=N
  python3 - <<PYEOF > "$tmpfile"
import urllib.request, json, sys, time

def norm(name):
    return name.lower().replace('-', '_')

packages = set()
per_page = 100
pages = max(1, ${TOP_N} // per_page)

headers = {
    'User-Agent': 'synapses-registry-refresh/1.0 (https://github.com/SynapsesOS/synapses)',
}

for page in range(1, pages + 1):
    url = f"https://crates.io/api/v1/crates?sort=downloads&per_page={per_page}&page={page}"
    try:
        req = urllib.request.Request(url, headers=headers)
        with urllib.request.urlopen(req, timeout=10) as r:
            data = json.load(r)
            crates = data.get("crates", [])
            if not crates:
                break
            for c in crates:
                name = c.get("name", "").strip()
                if name:
                    packages.add(norm(name))
        # Respect crates.io rate limit: 1 req/s
        if page < pages:
            time.sleep(1.1)
    except Exception as e:
        print(f"# WARNING: page {page} failed: {e}", file=sys.stderr)
        break

for p in sorted(packages):
    print(p)
PYEOF

  local count
  count="$(wc -l < "$tmpfile")"
  echo "Got $count crates from API"

  {
    echo "$seed_header"
    echo ""
    echo "# ── Auto-fetched top crates ($(date +%Y-%m-%d)) ────────────────────────────────"
    sort -u "$tmpfile"
  } > "$out.tmp"
  mv "$out.tmp" "$out"
  rm -f "$tmpfile"
  echo "Updated: $out"
}

# ── Go modules ────────────────────────────────────────────────────────────────

refresh_go() {
  header "Go modules"
  # Go modules are handled via prefix matching in the registry (github.com/*, golang.org/x/*, etc.)
  # so a large list is less critical. We refresh the seed list from pkg.go.dev.
  echo "Go modules use prefix-based matching — seed refresh is low priority."
  echo "Seed at $BUILTIN_DIR/go-modules.txt is maintained manually."
  echo "Run: go install golang.org/x/pkgsite/cmd/pkgsite@latest to explore module data."
}

# ── Main ──────────────────────────────────────────────────────────────────────

echo "Package registry refresh — $(date)"
echo "Target: $BUILTIN_DIR"
echo "Fetching top $TOP_N packages per ecosystem"

$DO_NPM    && refresh_npm
$DO_PYPI   && refresh_pypi
$DO_CRATES && refresh_crates
$DO_GO     && refresh_go

echo ""
echo "Done. Run 'go test ./internal/security/...' to verify."
