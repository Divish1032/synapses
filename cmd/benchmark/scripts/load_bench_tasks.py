#!/usr/bin/env python3
"""Load benchmark task dataset from HuggingFace and output JSONL to stdout."""
import argparse
import json
import os
import sys
import warnings

# Suppress all warnings before any imports that might trigger them.
warnings.filterwarnings("ignore")
os.environ["HF_HUB_DISABLE_PROGRESS_BARS"] = "1"
os.environ["TRANSFORMERS_NO_ADVISORY_WARNINGS"] = "1"

import logging
logging.disable(logging.WARNING)

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--split", default="lite", help="Dataset split: lite | fast | full")
    parser.add_argument("--task-ids", default="", help="Comma-separated task IDs to filter")
    parser.add_argument("--level", type=int, default=0, help="Level filter: 1 or 2 (0 = all)")
    args = parser.parse_args()

    from datasets import load_dataset

    ds = load_dataset("LiberCoders/FeatureBench", split=args.split)
    task_ids = set(t.strip() for t in args.task_ids.split(",") if t.strip()) if args.task_ids else None

    for row in ds:
        iid = row["instance_id"]

        # Level filter
        level = 1 if iid.endswith(".lv1") else 2 if iid.endswith(".lv2") else 0
        if args.level and level != args.level:
            continue

        # Task ID filter
        if task_ids and iid not in task_ids:
            continue

        obj = {
            "instance_id": iid,
            "repo": row["repo"],
            "base_commit": row["base_commit"],
            "problem_statement": row["problem_statement"],
            "image_name": row.get("image_name", ""),
            "repo_settings": row.get("repo_settings", "{}"),
            "patch": row.get("patch", ""),
            "test_patch": row.get("test_patch", ""),
            "FAIL_TO_PASS": row.get("FAIL_TO_PASS", []),
            "PASS_TO_PASS": row.get("PASS_TO_PASS", []),
        }
        print(json.dumps(obj))

if __name__ == "__main__":
    main()
