#!/usr/bin/env python3
"""Load SWE-bench Verified dataset from HuggingFace and output JSONL to stdout.

Matches the BenchTask struct format used by taskrunner.go.
"""
import argparse
import json
import os
import sys
import warnings

warnings.filterwarnings("ignore")
os.environ["HF_HUB_DISABLE_PROGRESS_BARS"] = "1"
os.environ["TRANSFORMERS_NO_ADVISORY_WARNINGS"] = "1"

import logging
logging.disable(logging.WARNING)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--limit", type=int, default=0, help="Max tasks (0 = all)")
    parser.add_argument("--instance-ids", default="", help="Comma-separated instance IDs to filter")
    parser.add_argument("--repos", default="", help="Comma-separated repo filters (e.g. 'astropy/astropy')")
    parser.add_argument("--dataset", default="princeton-nlp/SWE-bench_Verified",
                        help="HuggingFace dataset path")
    args = parser.parse_args()

    from datasets import load_dataset

    ds = load_dataset(args.dataset, split="test")

    instance_ids = set(t.strip() for t in args.instance_ids.split(",") if t.strip()) if args.instance_ids else None
    repos = set(r.strip() for r in args.repos.split(",") if r.strip()) if args.repos else None

    count = 0
    for row in ds:
        iid = row["instance_id"]

        # Instance ID filter.
        if instance_ids and iid not in instance_ids:
            continue

        # Repo filter.
        if repos and row["repo"] not in repos:
            continue

        # Parse FAIL_TO_PASS and PASS_TO_PASS — may be JSON strings or lists.
        f2p = row.get("FAIL_TO_PASS", [])
        if isinstance(f2p, str):
            try:
                f2p = json.loads(f2p)
            except json.JSONDecodeError:
                f2p = [f2p] if f2p else []

        p2p = row.get("PASS_TO_PASS", [])
        if isinstance(p2p, str):
            try:
                p2p = json.loads(p2p)
            except json.JSONDecodeError:
                p2p = [p2p] if p2p else []

        obj = {
            "instance_id": iid,
            "repo": row["repo"],
            "base_commit": row["base_commit"],
            "problem_statement": row["problem_statement"],
            "image_name": row.get("image_name", ""),
            "repo_settings": "{}",
            "patch": row.get("patch", ""),
            "test_patch": row.get("test_patch", ""),
            "FAIL_TO_PASS": f2p,
            "PASS_TO_PASS": p2p,
        }
        print(json.dumps(obj))

        count += 1
        if args.limit > 0 and count >= args.limit:
            break


if __name__ == "__main__":
    main()
