# FeatureBench Pilot — Sprint 30.8

## Overview

10-task FeatureBench validation to determine whether Synapses provides measurable value on hard feature implementation tasks before investing in Sprint 31+.

## Tasks Selected

| # | Instance ID | Repo | Domain |
|---|------------|------|--------|
| 1 | fastapi\_\_fastapi.02e108d1.test\_compat.71e8518f.lv1 | fastapi/fastapi | Web framework |
| 2 | mlflow\_\_mlflow.93dab383.test\_client.42de781c.lv1 | mlflow/mlflow | ML ops |
| 3 | pandas-dev\_\_pandas.82fa2715.test\_merge\_antijoin.921feefe.lv1 | pandas-dev/pandas | Data analysis |
| 4 | sympy\_\_sympy.c1097516.test\_nullspace.f14fc970.lv1 | sympy/sympy | Symbolic math |
| 5 | pytest-dev\_\_pytest.68016f0e.test\_local.40fb2f1f.lv1 | pytest-dev/pytest | Testing |
| 6 | pydantic\_\_pydantic.e1dcaf9e.test\_experimental\_arguments\_schema.00dc2dd4.lv1 | pydantic/pydantic | Validation |
| 7 | matplotlib\_\_matplotlib.86a476d2.test\_backend\_registry.872ba384.lv1 | matplotlib/matplotlib | Visualization |
| 8 | mwaskom\_\_seaborn.7001ebe7.test\_relational.f23eb542.lv1 | mwaskom/seaborn | Visualization |
| 9 | scikit-learn\_\_scikit-learn.5741bac9.test\_predict\_error\_display.11cc0c3a.lv1 | scikit-learn/scikit-learn | ML |
| 10 | sphinx-doc\_\_sphinx.e347e59c.test\_build\_html.d253ea54.lv1 | sphinx-doc/sphinx | Documentation |

Selection criteria: one task per repo, 10 different repos covering web, ML, data, math, testing, and docs domains.

## How to Run

### Prerequisites

```bash
# 1. Start Docker Desktop
open -a Docker

# 2. Install Python deps
pip install datasets swebench

# 3. Ensure claude CLI is on PATH
which claude  # should resolve

# 4. Build from repo root
cd /Users/itachi/Documents/Github/synapses-os/synapses
go build -o cmd/benchmark/benchmark ./cmd/benchmark
go build -o cmd/benchmark/synapses ./cmd/synapses
```

### Execute

```bash
# Full run (both modes, ~3-5 hours)
./cmd/benchmark/run_featurebench.sh

# Baseline only (~1.5-2.5 hours)
./cmd/benchmark/run_featurebench.sh --mode=baseline

# Synapses only (~1.5-2.5 hours)
./cmd/benchmark/run_featurebench.sh --mode=synapses

# With debug output
./cmd/benchmark/run_featurebench.sh --debug

# Skip Docker eval (faster, patches only)
./cmd/benchmark/run_featurebench.sh --no-eval
```

### Or use the benchmark binary directly

```bash
./cmd/benchmark/benchmark \
  --benchmark=taskbench \
  --tb-data=cmd/benchmark/featurebench_pilot.jsonl \
  --tb-dataset="LiberCoders/FeatureBench" \
  --tb-feature \
  --tb-both-modes \
  --tb-timeout=900 \
  --model=claude-sonnet-4-6 \
  --output-dir=cmd/benchmark/results/featurebench_pilot
```

## Interpretation Guide

### Gate Criteria (from ROADMAP)

| Outcome | Resolve Rate Delta | Action |
|---------|-------------------|--------|
| Strong positive | >= +2pp (>= +20% relative on 10 tasks, i.e. 2+ more resolved) | Proceed to Sprint 31 |
| Weak positive | +1pp with actionable failure modes | Fix failure modes, re-run |
| Inconclusive | < +1pp but Synapses-mode shows better exploration metrics | Investigate, possibly supplement with Go tasks |
| Negative | < +1pp, no actionable insights | Rethink architecture (see ROADMAP fallback plan) |

### What to Look For

1. **Resolve rate**: Primary metric. How many tasks pass all F2P tests AND all P2P tests.
2. **Exploration efficiency**: Does Synapses mode read fewer files while finding the right code?
3. **MCP tool usage**: Is the agent actually using Synapses tools? (avg 0.3 calls in prior internal test = barely used)
4. **Failure mode analysis**: For tasks that fail in baseline but pass with Synapses (or vice versa), what made the difference?
5. **Cost/turns**: Does Synapses mode cost more or less? Faster or slower?

### Python-Only Limitation

FeatureBench is Python-only (24 repos). Synapses is strongest in Go. If results are inconclusive, supplement with 5 Go feature tasks (manual or from SWE-bench-Verified Go repos).

## Results Location

Results written to `cmd/benchmark/results/featurebench_pilot/`:
- `taskbench_*.json` — Per-run machine-readable results
- `taskbench_*.md` — Per-run human-readable summary
- `taskbench_predictions_eval.jsonl` — Patches for Docker eval
