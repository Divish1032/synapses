#!/usr/bin/env python3
"""Analyze FeatureBench pilot results and produce the Sprint 30.8 verdict.

Usage:
    python3 analyze_featurebench.py --results-dir cmd/benchmark/results/featurebench_pilot
"""
import argparse
import json
import glob
import os
import sys


def load_results(results_dir):
    """Load baseline and synapses results from JSON files."""
    baseline = None
    synapses = None

    for path in sorted(glob.glob(os.path.join(results_dir, "taskbench_*.json"))):
        with open(path) as f:
            data = json.load(f)
        mode = data.get("mode", "")
        if mode == "baseline" and (baseline is None or path > baseline[1]):
            baseline = (data, path)
        elif mode == "synapses" and (synapses is None or path > synapses[1]):
            synapses = (data, path)

    return (baseline[0] if baseline else None,
            synapses[0] if synapses else None)


def analyze(baseline, synapses):
    """Produce the Sprint 30.8 verdict."""
    print("=" * 70)
    print("  FeatureBench Pilot — Sprint 30.8 Gate Analysis")
    print("=" * 70)
    print()

    if baseline is None:
        print("ERROR: No baseline results found.")
        return
    if synapses is None:
        print("ERROR: No synapses results found.")
        return

    b_total = baseline["total_tasks"]
    s_total = synapses["total_tasks"]
    b_resolved = baseline.get("resolved", 0)
    s_resolved = synapses.get("resolved", 0)
    b_rate = baseline.get("resolve_rate", 0)
    s_rate = synapses.get("resolve_rate", 0)
    b_patches = baseline.get("patch_count", 0)
    s_patches = synapses.get("patch_count", 0)

    delta_pp = s_rate - b_rate
    delta_abs = s_resolved - b_resolved

    print(f"  Baseline:  {b_resolved}/{b_total} resolved ({b_rate:.1f}%), "
          f"{b_patches} patches generated")
    print(f"  Synapses:  {s_resolved}/{s_total} resolved ({s_rate:.1f}%), "
          f"{s_patches} patches generated")
    print(f"  Delta:     {delta_pp:+.1f}pp ({delta_abs:+d} tasks)")
    print()

    # Cost comparison
    b_cost = baseline.get("total_cost_usd", 0)
    s_cost = synapses.get("total_cost_usd", 0)
    b_turns = baseline.get("avg_turns", 0)
    s_turns = synapses.get("avg_turns", 0)

    print(f"  Cost:      baseline=${b_cost:.2f}, synapses=${s_cost:.2f} "
          f"({((s_cost-b_cost)/b_cost*100) if b_cost > 0 else 0:+.1f}%)")
    print(f"  Avg turns: baseline={b_turns:.1f}, synapses={s_turns:.1f}")
    print()

    # Per-task comparison
    print("-" * 70)
    print(f"  {'Task':<55} {'Base':>5} {'Syn':>5}")
    print("-" * 70)

    b_tasks = {t["instance_id"]: t for t in baseline.get("tasks", [])}
    s_tasks = {t["instance_id"]: t for t in synapses.get("tasks", [])}

    all_ids = sorted(set(list(b_tasks.keys()) + list(s_tasks.keys())))
    flipped_to_pass = []
    flipped_to_fail = []

    for iid in all_ids:
        bt = b_tasks.get(iid, {})
        st = s_tasks.get(iid, {})
        b_res = "PASS" if bt.get("resolved") else "FAIL"
        s_res = "PASS" if st.get("resolved") else "FAIL"
        marker = ""
        if b_res == "FAIL" and s_res == "PASS":
            marker = " ← GAINED"
            flipped_to_pass.append(iid)
        elif b_res == "PASS" and s_res == "FAIL":
            marker = " ← LOST"
            flipped_to_fail.append(iid)

        short_id = iid[:53] if len(iid) > 53 else iid
        print(f"  {short_id:<55} {b_res:>5} {s_res:>5}{marker}")

    print("-" * 70)
    print()

    # MCP usage analysis
    s_mcp_counts = []
    for t in synapses.get("tasks", []):
        mcp_total = sum(v for k, v in t.get("tool_calls", {}).items()
                       if "mcp__synapses" in k)
        s_mcp_counts.append(mcp_total)

    avg_mcp = sum(s_mcp_counts) / len(s_mcp_counts) if s_mcp_counts else 0
    tasks_using_mcp = sum(1 for c in s_mcp_counts if c > 0)

    print(f"  MCP Usage: {tasks_using_mcp}/{len(s_mcp_counts)} tasks used Synapses tools")
    print(f"  Avg MCP calls per task: {avg_mcp:.1f}")
    if avg_mcp < 1:
        print("  WARNING: Agent barely used Synapses tools — results may not reflect tool value")
    print()

    # Failure mode analysis
    if flipped_to_pass:
        print("  Tasks GAINED with Synapses:")
        for iid in flipped_to_pass:
            st = s_tasks[iid]
            mcp = sum(v for k, v in st.get("tool_calls", {}).items()
                     if "mcp__synapses" in k)
            print(f"    {iid}")
            print(f"      MCP calls: {mcp}, turns: {st.get('turns', '?')}")
        print()

    if flipped_to_fail:
        print("  Tasks LOST with Synapses:")
        for iid in flipped_to_fail:
            st = s_tasks[iid]
            err = st.get("error", "none")
            mcp_ok = st.get("mcp_connected", False)
            print(f"    {iid}")
            print(f"      MCP connected: {mcp_ok}, error: {err}")
        print()

    # Verdict
    print("=" * 70)
    print("  VERDICT")
    print("=" * 70)
    print()

    if delta_pp >= 2.0:
        print("  STRONG POSITIVE: Delta >= +2pp")
        print("  → Proceed to Sprint 31")
    elif delta_pp >= 1.0:
        print("  WEAK POSITIVE: Delta >= +1pp")
        if flipped_to_pass:
            print("  → Actionable improvement found. Fix failure modes and re-run.")
        else:
            print("  → Marginal. Investigate MCP usage and failure modes.")
    elif delta_pp > 0:
        print("  MARGINAL: 0 < delta < +1pp")
        print("  → Investigate. Synapses may help on specific task types.")
    elif delta_pp == 0:
        print("  NEUTRAL: No difference detected")
        if avg_mcp < 1:
            print("  → Agent didn't use tools. Fix tool adoption before re-running.")
        else:
            print("  → Tools used but no impact. Rethink what intelligence to provide.")
    else:
        print("  NEGATIVE: Synapses mode performed worse")
        print("  → Investigate: MCP overhead? Wrong context? Distraction?")

    print()

    # Note about Docker eval
    if not any(t.get("resolved") for t in baseline.get("tasks", [])):
        if all(t.get("model_patch", "") for t in baseline.get("tasks", [])):
            print("  NOTE: All tasks show resolved=false but patches were generated.")
            print("  This likely means Docker eval was not run. Run with --tb-eval")
            print("  to get ground-truth resolve status.")
            print()

    # Python-only limitation
    print("  NOTE: All tasks are Python. Synapses is strongest in Go.")
    print("  If inconclusive, supplement with 5 Go feature tasks.")
    print()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--results-dir", required=True)
    args = parser.parse_args()

    baseline, synapses = load_results(args.results_dir)
    analyze(baseline, synapses)


if __name__ == "__main__":
    main()
