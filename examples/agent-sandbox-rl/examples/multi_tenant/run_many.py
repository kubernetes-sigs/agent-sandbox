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

"""Run several users' experiments at once and print one table.

The demo driver: for every user in ``--users`` and every experiment number up
to ``--experiments``, it starts ``run_experiment.py`` as its own process, all
at the same time, each with the user's kubeconfig. When they finish it reads
the reports and prints, per run, the claims made, how long a claim took at
the median and at the slowest, task outcomes and whether the teardown left
nothing behind. Two users on the same images show the same claim times after
the admin's pre-pull, because the image layers are already on the nodes.

  python run_many.py --users alice,bob --experiments 2 \\
      --kubeconfig-pattern '~/.kube/rl-{user}.kubeconfig' --report-dir reports
"""

from __future__ import annotations

import argparse
import json
import pathlib
import subprocess
import sys

HERE = pathlib.Path(__file__).resolve().parent


def summarize(reports: list[dict]) -> list[dict]:
  """One row per report, in (user, experiment) order."""
  rows = []
  for doc in sorted(reports, key=lambda d: (d.get("user", ""), d.get("experiment", ""))):
    report = doc.get("report") or {}
    claim = (report.get("phases") or {}).get("claim") or {}
    n = report.get("claims") or 0
    timed = claim.get("count") or 0
    rows.append({
        "user": doc.get("user", "?"),
        "experiment": doc.get("experiment", "?"),
        "namespace": doc.get("namespace", "?"),
        "run_id": doc.get("run_id", "?"),
        "claims": n,
        "claim_avg_s": round(claim.get("total_s", 0.0) / timed, 2) if timed else None,
        "claim_max_s": claim.get("max_s"),
        "tasks": doc.get("tasks", 0),
        "task_errors": doc.get("task_errors", 0),
        "total_s": (doc.get("report") or {}).get("total_s"),
        "teardown_clean": bool(doc.get("teardown_clean")),
    })
  return rows


def render_table(rows: list[dict]) -> str:
  cols = [("user", "user"), ("experiment", "experiment"), ("namespace", "namespace"),
          ("claims", "claims"), ("claim_avg_s", "claim avg s"), ("claim_max_s", "claim max s"),
          ("tasks", "tasks"), ("task_errors", "errors"), ("total_s", "total s"),
          ("teardown_clean", "teardown clean")]
  table = [[h for _, h in cols]]
  for r in rows:
    table.append(["" if r.get(k) is None else str(r.get(k)) for k, _ in cols])
  widths = [max(len(row[i]) for row in table) for i in range(len(cols))]
  lines = []
  for n, row in enumerate(table):
    lines.append("  ".join(c.ljust(w) for c, w in zip(row, widths)).rstrip())
    if n == 0:
      lines.append("  ".join("-" * w for w in widths))
  return "\n".join(lines)


def main(argv=None) -> int:
  p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
  p.add_argument("--users", required=True, help="comma-separated tenant names")
  p.add_argument("--experiments", type=int, default=2, help="experiments per user")
  p.add_argument("--kubeconfig-pattern", default="~/.kube/rl-{user}.kubeconfig",
                 help="per-user kubeconfig; '' runs every user as the current identity")
  p.add_argument("--report-dir", default="reports")
  p.add_argument("--python", default=sys.executable)
  p.add_argument("runner_args", nargs=argparse.REMAINDER,
                 help="everything after -- goes to run_experiment.py")
  args = p.parse_args(argv)
  extra = [a for a in args.runner_args if a != "--"]
  users = [u for u in args.users.split(",") if u]
  if not users:
    p.error("--users is empty")

  procs = []
  for user in users:
    for n in range(1, args.experiments + 1):
      # All of a user's experiments start at once and share the namespace
      # quota, so each fits itself into 1/N of what is free.
      cmd = [args.python, str(HERE / "run_experiment.py"), "--user", user,
             "--experiment", f"exp{n}", "--report-dir", args.report_dir,
             "--share", str(args.experiments), *extra]
      if args.kubeconfig_pattern:
        cmd += ["--kubeconfig",
                str(pathlib.Path(args.kubeconfig_pattern.format(user=user)).expanduser())]
      log = pathlib.Path(args.report_dir) / user / f"exp{n}.log"
      log.parent.mkdir(parents=True, exist_ok=True)
      print(f"starting {user}/exp{n} (log: {log})")
      procs.append((user, f"exp{n}", log, subprocess.Popen(
          cmd, stdout=log.open("w"), stderr=subprocess.STDOUT)))

  failed = 0
  for user, exp, log, proc in procs:
    rc = proc.wait()
    if rc != 0:
      failed += 1
      print(f"{user}/{exp}: exit {rc} (see {log})")

  reports = []
  for path in sorted(pathlib.Path(args.report_dir).glob("*/*/report.json")):
    try:
      reports.append(json.loads(path.read_text()))
    except (OSError, ValueError) as e:
      print(f"unreadable report {path}: {e}")
      failed += 1
  print()
  print(render_table(summarize(reports)))
  return 1 if failed else 0


if __name__ == "__main__":
  sys.exit(main())
