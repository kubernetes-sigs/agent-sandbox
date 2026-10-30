# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Summarize a Harbor job directory as a markdown table.

Reads every ``result.json`` under the job directory (one per trial), prints
per-task reward, outcome and the environment setup, agent and verifier
durations, and a summary line. Standard library only, so it runs anywhere
``harbor`` ran.

    python scripts/report.py jobs/harbor-terminal-bench [--json]
"""

from __future__ import annotations

import argparse
import json
import sys
from datetime import datetime
from pathlib import Path
from typing import Any


def _seconds(timing: dict[str, Any] | None) -> float | None:
    if not timing or not timing.get("started_at") or not timing.get("finished_at"):
        return None
    start = datetime.fromisoformat(timing["started_at"])
    end = datetime.fromisoformat(timing["finished_at"])
    return round((end - start).total_seconds(), 1)


def _reward(result: dict[str, Any]) -> float | None:
    rewards = (result.get("verifier_result") or {}).get("rewards") or {}
    if not rewards:
        return None
    if "reward" in rewards:
        return float(rewards["reward"])
    return float(max(rewards.values()))


def summarize_trial(result: dict[str, Any]) -> dict[str, Any]:
    """One row per trial: what the reader needs to judge a run."""
    exc = result.get("exception_info") or {}
    reward = _reward(result)
    if exc:
        outcome = f"error: {exc.get('exception_type', 'unknown')}"
    elif reward is None:
        outcome = "no reward"
    else:
        outcome = "pass" if reward >= 1 else "fail"
    return {
        "task": result.get("task_name", "?"),
        "trial": result.get("trial_name", "?"),
        "reward": reward,
        "outcome": outcome,
        "env_setup_s": _seconds(result.get("environment_setup")),
        "agent_s": _seconds(result.get("agent_execution")),
        "verifier_s": _seconds(result.get("verifier")),
    }


def load_rows(job_dir: Path) -> list[dict[str, Any]]:
    rows = []
    for path in sorted(job_dir.rglob("result.json")):
        try:
            rows.append(summarize_trial(json.loads(path.read_text())))
        except (OSError, ValueError) as exc:
            rows.append({"task": path.parent.name, "trial": path.parent.name, "reward": None,
                         "outcome": f"unreadable result.json: {exc}", "env_setup_s": None,
                         "agent_s": None, "verifier_s": None})
    return rows


def summary(rows: list[dict[str, Any]]) -> dict[str, Any]:
    n = len(rows)
    passed = sum(1 for r in rows if r["outcome"] == "pass")
    errors = sum(1 for r in rows if str(r["outcome"]).startswith("error"))
    setups = sorted(r["env_setup_s"] for r in rows if r["env_setup_s"] is not None)

    def pct(p: float) -> float | None:
        if not setups:
            return None
        return setups[min(len(setups) - 1, int(round(p * (len(setups) - 1))))]

    return {"trials": n, "passed": passed, "errors": errors,
            "pass_rate": round(passed / n, 3) if n else None,
            "env_setup_p50_s": pct(0.5), "env_setup_p95_s": pct(0.95)}


def _fmt(value: Any) -> str:
    return "-" if value is None else (f"{value:g}" if isinstance(value, float) else str(value))


def render(rows: list[dict[str, Any]]) -> str:
    lines = ["| task | trial | reward | outcome | env setup s | agent s | verifier s |",
             "|---|---|---|---|---|---|---|"]
    for r in rows:
        lines.append(f"| {r['task']} | {r['trial']} | {_fmt(r['reward'])} | {r['outcome']} | "
                     f"{_fmt(r['env_setup_s'])} | {_fmt(r['agent_s'])} | {_fmt(r['verifier_s'])} |")
    s = summary(rows)
    lines.append("")
    lines.append(f"{s['passed']}/{s['trials']} passed, {s['errors']} infrastructure errors, "
                 f"env setup p50 {_fmt(s['env_setup_p50_s'])} s / p95 {_fmt(s['env_setup_p95_s'])} s")
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("job_dir", type=Path, help="a Harbor job directory (or a jobs_dir with several)")
    parser.add_argument("--json", action="store_true", help="print rows and summary as JSON")
    args = parser.parse_args(argv)
    if not args.job_dir.is_dir():
        print(f"{args.job_dir} is not a directory", file=sys.stderr)
        return 2
    rows = load_rows(args.job_dir)
    if not rows:
        print(f"no result.json under {args.job_dir}", file=sys.stderr)
        return 1
    if args.json:
        print(json.dumps({"trials": rows, "summary": summary(rows)}, indent=2))
    else:
        print(render(rows))
    return 0


if __name__ == "__main__":
    sys.exit(main())
