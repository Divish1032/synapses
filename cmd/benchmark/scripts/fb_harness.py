#!/usr/bin/env python3
"""Lightweight FeatureBench harness.

Agent runs on HOST (uses Max subscription). Tests run in Docker (clean environment).
No 11GB images — builds minimal containers from repo requirements.

Usage:
    python3 fb_harness.py --task-file featurebench_pilot.jsonl --mode baseline --limit 1
    python3 fb_harness.py --task-file featurebench_pilot.jsonl --mode synapses --limit 1
    python3 fb_harness.py --task-file featurebench_pilot.jsonl --mode both
"""
import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path


def load_tasks(path, task_ids=None, limit=0):
    tasks = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            task = json.loads(line)
            if task_ids and task['instance_id'] not in task_ids:
                continue
            tasks.append(task)
    if limit > 0:
        tasks = tasks[:limit]
    return tasks


def clone_repo(repo, commit, dest):
    """Clone repo at specific commit."""
    url = f"https://github.com/{repo}.git"
    subprocess.run(["git", "clone", "--quiet", url, dest],
                   check=True, capture_output=True)
    subprocess.run(["git", "checkout", "--quiet", commit],
                   cwd=dest, check=True, capture_output=True)


def apply_patch(repo_dir, patch_text):
    """Apply a git patch (test patch or gold patch)."""
    if not patch_text:
        return
    proc = subprocess.run(
        ["git", "apply", "--allow-empty", "-"],
        input=patch_text.encode(),
        cwd=repo_dir,
        capture_output=True
    )
    if proc.returncode != 0:
        # Try with git apply --3way
        proc2 = subprocess.run(
            ["git", "apply", "--3way", "-"],
            input=patch_text.encode(),
            cwd=repo_dir,
            capture_output=True
        )
        if proc2.returncode != 0:
            print(f"  WARNING: patch apply failed: {proc2.stderr.decode()[:200]}")


def git_commit_all(repo_dir, msg):
    subprocess.run(["git", "add", "-A"], cwd=repo_dir, capture_output=True)
    subprocess.run(["git", "commit", "--allow-empty", "-m", msg],
                   cwd=repo_dir, capture_output=True)


def get_settings(task):
    """Parse repo_settings JSON."""
    raw = task.get('repo_settings', '{}')
    if isinstance(raw, str):
        return json.loads(raw) if raw else {}
    return raw or {}


def build_test_docker_image(task, repo_dir):
    """Build a minimal Docker image for running tests."""
    settings = get_settings(task)
    base = settings.get('base_image', 'python311')

    # Map base_image to real Docker image
    python_map = {
        'python310': 'python:3.10-slim',
        'python311': 'python:3.11-slim',
        'python312': 'python:3.12-slim',
        'python310_cu121_torch28': 'python:3.10-slim',  # skip GPU
    }
    docker_base = python_map.get(base, 'python:3.11-slim')

    install_cmd = settings.get('install', 'pip install -e .')
    pip_packages = settings.get('pip_packages', [])

    # Build Dockerfile
    pip_line = ""
    if pip_packages:
        pkgs = " ".join(f"'{p}'" for p in pip_packages)
        pip_line = f"RUN pip install --no-cache-dir {pkgs}"

    dockerfile = f"""FROM {docker_base}
WORKDIR /testbed
COPY . /testbed/
RUN pip install --upgrade pip setuptools wheel
{pip_line}
RUN {install_cmd} || true
"""
    dockerfile_path = os.path.join(repo_dir, "Dockerfile.fbtest")
    with open(dockerfile_path, 'w') as f:
        f.write(dockerfile)

    tag = f"fb-test-{task['repo'].replace('/', '-').lower()}:latest"

    print(f"  building test image ({docker_base})...")
    proc = subprocess.run(
        ["docker", "build", "-f", "Dockerfile.fbtest", "-t", tag, "."],
        cwd=repo_dir,
        capture_output=True,
        timeout=600
    )
    if proc.returncode != 0:
        stderr = proc.stderr.decode()[-500:]
        print(f"  WARNING: docker build failed: {stderr}")
        return None
    return tag


def run_tests_in_docker(image_tag, repo_dir, test_files, settings, timeout=300):
    """Run specific test files inside Docker. Returns (passed_count, total_count, output)."""
    test_cmd = settings.get('test_cmd', 'pytest -rA --tb=short --color=no')

    results = []
    for tf in test_files:
        cmd = f"{test_cmd} {tf}"
        try:
            proc = subprocess.run(
                ["docker", "run", "--rm",
                 "-v", f"{repo_dir}:/testbed",
                 "-w", "/testbed",
                 image_tag,
                 "bash", "-c", cmd],
                capture_output=True,
                timeout=timeout
            )
            passed = proc.returncode == 0
            output = proc.stdout.decode()[-500:] + proc.stderr.decode()[-200:]
            results.append({
                'test_file': tf,
                'passed': passed,
                'exit_code': proc.returncode,
                'output': output
            })
        except subprocess.TimeoutExpired:
            results.append({
                'test_file': tf,
                'passed': False,
                'exit_code': -1,
                'output': 'TIMEOUT'
            })
    return results


def run_claude(repo_dir, prompt, mode, model, synapses_bin, timeout):
    """Run Claude Code on the task. Returns stream-json output."""
    claude_bin = shutil.which("claude")
    if not claude_bin:
        # Try common locations
        for p in [os.path.expanduser("~/.npm-global/bin/claude"),
                  "/usr/local/bin/claude"]:
            if os.path.isfile(p):
                claude_bin = p
                break
    if not claude_bin:
        print("  ERROR: claude not found")
        return ""

    allowed = "Bash Read Write Edit Grep Glob"
    system_prompt = ""
    env = os.environ.copy()

    if mode == "synapses":
        # Set up MCP + hooks
        setup_synapses(repo_dir, synapses_bin)
        allowed = "Bash Read Write Edit mcp__synapses__session_init mcp__synapses__search mcp__synapses__get_context mcp__synapses__get_impact mcp__synapses__validate"
        system_prompt = (
            "You have Synapses MCP tools. Call mcp__synapses__session_init first, "
            "then use mcp__synapses__search INSTEAD OF Grep/Glob. "
            "Use mcp__synapses__get_context for entity relationships."
        )

    args = [claude_bin, "-p", prompt,
            "--allowedTools", allowed,
            "--output-format", "stream-json",
            "--verbose",
            "--model", model,
            "--max-turns", "50"]

    if system_prompt:
        args.extend(["--system-prompt", system_prompt])

    try:
        proc = subprocess.run(
            args, cwd=repo_dir,
            capture_output=True, timeout=timeout,
            env=env, stdin=subprocess.DEVNULL
        )
        return proc.stdout.decode()
    except subprocess.TimeoutExpired:
        return ""


def setup_synapses(repo_dir, synapses_bin):
    """Index repo + set up MCP config for Synapses mode."""
    if not synapses_bin:
        synapses_bin = os.path.expanduser("~/.synapses/bin/synapses")

    # Index
    print(f"  indexing with Synapses...")
    subprocess.run([synapses_bin, "index", "--path", repo_dir],
                   capture_output=True, timeout=300)

    # MCP config
    mcp_json = json.dumps({
        "mcpServers": {
            "synapses": {
                "type": "stdio",
                "command": synapses_bin,
                "args": ["start", "--path", repo_dir]
            }
        }
    })
    with open(os.path.join(repo_dir, ".mcp.json"), 'w') as f:
        f.write(mcp_json)

    # Claude settings
    claude_dir = os.path.join(repo_dir, ".claude")
    os.makedirs(claude_dir, exist_ok=True)
    settings = json.dumps({
        "permissions": {"allow": ["mcp__synapses__*", "Bash(*)", "Read(*)", "Write(*)", "Edit(*)"]}
    })
    with open(os.path.join(claude_dir, "settings.json"), 'w') as f:
        f.write(settings)


def parse_agent_stats(stream_json):
    """Extract turns, cost, tool calls from stream-json."""
    stats = {'turns': 0, 'cost': 0, 'tool_calls': {}, 'mcp_connected': False}
    for line in stream_json.split('\n'):
        if not line.strip():
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError:
            continue

        if msg.get('type') == 'system' and 'mcp_servers' in msg:
            for s in msg.get('mcp_servers', []):
                if s.get('name') == 'synapses' and s.get('status') in ('connected', 'pending'):
                    stats['mcp_connected'] = True

        if msg.get('type') == 'assistant':
            for block in msg.get('message', {}).get('content', []):
                if block.get('type') == 'tool_use':
                    name = block.get('name', '')
                    stats['tool_calls'][name] = stats['tool_calls'].get(name, 0) + 1

        if msg.get('type') == 'result':
            stats['turns'] = msg.get('num_turns', 0)
            stats['cost'] = msg.get('total_cost_usd', 0)

    return stats


def run_task(task, mode, model, synapses_bin, repos_dir, timeout, docker_timeout):
    """Run a single task end-to-end. Returns result dict."""
    iid = task['instance_id']
    repo = task['repo']
    commit = task['base_commit']
    settings = get_settings(task)
    short_id = iid[:50]

    result = {
        'instance_id': iid,
        'repo': repo,
        'mode': mode,
        'resolved': False,
        'f2p_passed': 0,
        'f2p_total': len(task.get('FAIL_TO_PASS', [])),
        'p2p_passed': 0,
        'p2p_total': len(task.get('PASS_TO_PASS', [])),
        'turns': 0,
        'cost': 0,
        'tool_calls': {},
        'mcp_connected': False,
        'error': '',
    }

    # 1. Clone
    safe_name = repo.replace('/', '_')
    repo_dir = os.path.join(repos_dir, f"{safe_name}_{mode}")
    if os.path.exists(repo_dir):
        shutil.rmtree(repo_dir)

    print(f"  [{mode}] cloning {repo}...")
    try:
        clone_repo(repo, commit, repo_dir)
    except Exception as e:
        result['error'] = f"clone: {e}"
        return result

    # 2. Apply test patch (adds the feature tests)
    apply_patch(repo_dir, task.get('test_patch', ''))
    git_commit_all(repo_dir, "test baseline")

    # 3. Build Docker test image BEFORE agent runs
    # (so the image has the test files but NOT the feature code)
    image_tag = build_test_docker_image(task, repo_dir)
    if not image_tag:
        result['error'] = "docker build failed"
        return result

    # 4. Verify F2P tests FAIL before agent (sanity check)
    f2p_before = run_tests_in_docker(
        image_tag, repo_dir, task.get('FAIL_TO_PASS', []), settings, docker_timeout)
    f2p_failing = sum(1 for r in f2p_before if not r['passed'])
    print(f"  pre-agent: {f2p_failing}/{len(f2p_before)} F2P tests failing (expected)")

    # 5. Build prompt
    prompt = (
        f"You are an expert software engineer. Implement the following feature "
        f"in the {repo} repository.\n\n"
        f"The repository is in the CURRENT WORKING DIRECTORY. "
        f"Do NOT look for /testbed/.\n\n"
        f"## Feature Request\n\n{task['problem_statement']}\n\n"
        f"## Instructions\n"
        f"1. Read relevant source code\n"
        f"2. Implement the feature using Write/Edit tools\n"
        f"3. Do NOT modify test files\n"
        f"4. Do NOT run git commit\n"
    )

    # 6. Run agent
    print(f"  [{mode}] running Claude...")
    start = time.time()
    stream = run_claude(repo_dir, prompt, mode, model, synapses_bin, timeout)
    elapsed = time.time() - start
    print(f"  agent finished in {elapsed:.0f}s")

    stats = parse_agent_stats(stream)
    result['turns'] = stats['turns']
    result['cost'] = stats['cost']
    result['tool_calls'] = stats['tool_calls']
    result['mcp_connected'] = stats['mcp_connected']

    # 7. Rebuild Docker image WITH agent's changes
    image_tag_after = build_test_docker_image(task, repo_dir)
    if not image_tag_after:
        result['error'] = "docker rebuild failed"
        return result

    # 8. Run F2P tests (must now PASS)
    print(f"  running F2P tests...")
    f2p_results = run_tests_in_docker(
        image_tag_after, repo_dir, task.get('FAIL_TO_PASS', []), settings, docker_timeout)
    result['f2p_passed'] = sum(1 for r in f2p_results if r['passed'])

    # 9. Run P2P tests (must still PASS)
    print(f"  running P2P tests...")
    p2p_results = run_tests_in_docker(
        image_tag_after, repo_dir, task.get('PASS_TO_PASS', []), settings, docker_timeout)
    result['p2p_passed'] = sum(1 for r in p2p_results if r['passed'])

    # 10. Resolved = ALL F2P pass AND ALL P2P pass
    result['resolved'] = (
        result['f2p_passed'] == result['f2p_total'] and
        result['p2p_passed'] == result['p2p_total']
    )

    status = "RESOLVED" if result['resolved'] else "FAILED"
    print(f"  {status}: F2P {result['f2p_passed']}/{result['f2p_total']}, "
          f"P2P {result['p2p_passed']}/{result['p2p_total']}, "
          f"turns={result['turns']}, cost=${result['cost']:.2f}")

    # Cleanup
    subprocess.run(["docker", "rmi", "-f", image_tag], capture_output=True)
    if image_tag_after != image_tag:
        subprocess.run(["docker", "rmi", "-f", image_tag_after], capture_output=True)

    return result


def main():
    parser = argparse.ArgumentParser(description="Lightweight FeatureBench harness")
    parser.add_argument("--task-file", required=True, help="JSONL task file")
    parser.add_argument("--mode", default="both", choices=["baseline", "synapses", "both"])
    parser.add_argument("--model", default="claude-sonnet-4-6")
    parser.add_argument("--limit", type=int, default=0, help="Max tasks (0=all)")
    parser.add_argument("--task-ids", default="", help="Comma-separated task IDs")
    parser.add_argument("--timeout", type=int, default=900, help="Agent timeout per task")
    parser.add_argument("--docker-timeout", type=int, default=300, help="Test timeout")
    parser.add_argument("--repos-dir", default="/tmp/fb_repos")
    parser.add_argument("--output-dir", default="cmd/benchmark/results/featurebench_harness")
    parser.add_argument("--synapses-bin", default="")
    args = parser.parse_args()

    task_ids = set(t.strip() for t in args.task_ids.split(",") if t.strip()) if args.task_ids else None
    tasks = load_tasks(args.task_file, task_ids, args.limit)
    print(f"Loaded {len(tasks)} tasks")

    os.makedirs(args.output_dir, exist_ok=True)
    os.makedirs(args.repos_dir, exist_ok=True)

    modes = ["baseline", "synapses"] if args.mode == "both" else [args.mode]

    all_results = {}
    for mode in modes:
        print(f"\n{'='*60}")
        print(f"  MODE: {mode}")
        print(f"{'='*60}")
        results = []
        for i, task in enumerate(tasks):
            print(f"\nTask {i+1}/{len(tasks)}: {task['instance_id'][:50]}")
            r = run_task(task, mode, args.model, args.synapses_bin,
                        args.repos_dir, args.timeout, args.docker_timeout)
            results.append(r)

        all_results[mode] = results

        # Write per-mode results
        ts = time.strftime("%Y%m%d-%H%M%S")
        out_path = os.path.join(args.output_dir, f"{ts}-{mode}.json")
        with open(out_path, 'w') as f:
            json.dump({
                'mode': mode,
                'model': args.model,
                'total': len(results),
                'resolved': sum(1 for r in results if r['resolved']),
                'tasks': results,
            }, f, indent=2)
        print(f"\nResults: {out_path}")

    # Print comparison if both modes
    if len(all_results) == 2:
        print(f"\n{'='*60}")
        print(f"  COMPARISON")
        print(f"{'='*60}")
        for mode in modes:
            res = all_results[mode]
            resolved = sum(1 for r in res if r['resolved'])
            total = len(res)
            cost = sum(r['cost'] for r in res)
            turns = sum(r['turns'] for r in res) / max(len(res), 1)
            mcp = sum(sum(v for k,v in r['tool_calls'].items() if 'mcp' in k) for r in res)
            print(f"  {mode:>10}: {resolved}/{total} resolved, "
                  f"avg_turns={turns:.1f}, cost=${cost:.2f}, mcp_calls={mcp}")

        # Per-task delta
        b_map = {r['instance_id']: r for r in all_results['baseline']}
        s_map = {r['instance_id']: r for r in all_results['synapses']}
        print(f"\n  {'Task':<45} {'Base':>6} {'Syn':>6}")
        print(f"  {'-'*60}")
        for iid in sorted(b_map):
            b = "PASS" if b_map[iid]['resolved'] else "FAIL"
            s = "PASS" if s_map.get(iid, {}).get('resolved') else "FAIL"
            marker = ""
            if b == "FAIL" and s == "PASS":
                marker = " <- GAINED"
            elif b == "PASS" and s == "FAIL":
                marker = " <- LOST"
            print(f"  {iid[:44]:<45} {b:>6} {s:>6}{marker}")


if __name__ == "__main__":
    main()
