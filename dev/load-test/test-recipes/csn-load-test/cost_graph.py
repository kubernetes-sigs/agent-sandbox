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

"""
Cost comparison for CSN vs static runs from node-states.csv.

node-states.csv columns: timestamp,total_nodes,suspended,resumed_prev_suspended
running = total_nodes - suspended. Cost is node-hours: running at full price,
suspended at the suspended fraction (CSN guide: 2-4% of active). Same machine
type in both runs → the node-hours ratio IS the cost ratio (price cancels).

Two modes:
  # A) compare two real runs (CSN vs static):
  python3 cost_graph.py <csn>/node-states.csv --compare <static>/node-states.csv

  # B) compare one run against a flat always-on baseline:
  python3 cost_graph.py <csn>/node-states.csv --static-nodes 135

Options: --price <$/node-hr> (default 1 = pure node-hours), --suspended-frac
0.03, --out cost_graph.png
"""

import argparse
import csv
import sys


def load(path):
    rows = []
    with open(path) as f:
        for r in csv.DictReader(f):
            rows.append((int(r["timestamp"]), int(r["total_nodes"]),
                         int(r["suspended"])))
    rows.sort()
    if len(rows) < 2:
        sys.exit(f"{path}: need >= 2 rows")
    return rows


def node_hours(rows, price, susp_frac):
    """Step-integrate node-hours. Returns running/suspended/weighted hrs + stats."""
    runh = susph = secs = 0.0
    peak_run = max(total - susp for _, total, susp in rows)
    peak_total = max(total for _, total, _ in rows)
    for i in range(1, len(rows)):
        ts, total, susp = rows[i - 1]
        dt = rows[i][0] - ts
        run = total - susp
        runh += run * dt / 3600.0
        susph += susp * dt / 3600.0
        secs += dt
    weighted = runh * price + susph * price * susp_frac
    return dict(runh=runh, susph=susph, weighted=weighted, secs=secs,
                peak_run=peak_run, peak_total=peak_total,
                avg_run=runh / (secs / 3600.0) if secs else 0)


def summarize(name, m, price):
    print(f"  {name}:")
    print(f"    duration {m['secs']/60:.1f} min | peak running {m['peak_run']} "
          f"| avg running {m['avg_run']:.0f} | peak total {m['peak_total']}")
    if price == 1:
        print(f"    running {m['runh']:.1f} + suspended {m['susph']:.1f}@frac "
              f"=> weighted {m['weighted']:.1f} node-hrs")
    else:
        print(f"    running {m['runh']:.1f} node-hrs + suspended {m['susph']:.1f}@frac "
              f"=> weighted ${m['weighted']:.1f}")


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("node_states", help="CSN run node-states.csv")
    mode = p.add_mutually_exclusive_group(required=True)
    mode.add_argument("--compare", help="static run node-states.csv (compare mode)")
    mode.add_argument("--static-nodes", type=int,
                      help="flat always-on baseline node count")
    p.add_argument("--price", type=float, default=1.0,
                   help="$/node-hr; default 1 => report pure node-hours")
    p.add_argument("--suspended-frac", type=float, default=0.03)
    p.add_argument("--out", default="cost_graph.png")
    args = p.parse_args()
    if args.static_nodes is not None and args.static_nodes <= 0:
        p.error("--static-nodes must be > 0")
    if args.price <= 0:
        p.error("--price must be > 0")
    if not 0 <= args.suspended_frac <= 1:
        p.error("--suspended-frac must be between 0 and 1")

    csn_rows = load(args.node_states)
    csn = node_hours(csn_rows, args.price, args.suspended_frac)

    print("=== Cost comparison (node-hours; same machine type => = cost) ===")
    summarize("CSN", csn, args.price)

    csn_weighted_for_ratio = csn["weighted"]
    if args.compare:
        static_rows = load(args.compare)
        static = node_hours(static_rows, args.price, args.suspended_frac)
        summarize("STATIC", static, args.price)
        # Normalize compare-mode to rate to avoid duration bias.
        csn_rate = csn["weighted"] / (csn["secs"] / 3600.0) if csn["secs"] else 0.0
        static_rate = static["weighted"] / (static["secs"] / 3600.0) if static["secs"] else 0.0
        baseline = static_rate
        csn_weighted_for_ratio = csn_rate
        static_label = f"static run ({static['peak_run']} peak running)"
    else:
        baseline = args.static_nodes * args.price * (csn["secs"] / 3600.0)
        if args.price == 1:
            print(f"  STATIC (flat {args.static_nodes} always-on): {baseline:.1f} node-hrs")
        else:
            print(f"  STATIC (flat {args.static_nodes} always-on): ${baseline:.1f}")
        static_rows = None
        static_label = f"static {args.static_nodes} always-on"

    ratio = 100 * csn_weighted_for_ratio / baseline if baseline else 0
    print("\n=== COST EFFICIENCY ===")
    print(f"  CSN is {ratio:.0f}% of static cost  =>  {100-ratio:.0f}% cheaper")
    if csn_weighted_for_ratio > baseline:
        print("  ⚠ CSN cost EXCEEDS static — likely over-provisioning (cleanup lag) "
              "and/or the idle scale-down/re-suspension was not captured. Not a "
              "valid savings run; ensure node count falls during the idle tail.")
    if args.compare and static_rows and static["peak_run"] != csn["peak_run"]:
        print(f"  note: peak running differs (CSN {csn['peak_run']} vs static "
              f"{static['peak_run']}). For a fair baseline, static should be sized "
              f"to ~the CSN peak running, else the % is skewed.")

    # --- plot ---
    try:
        import matplotlib
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
    except ImportError:
        print(f"\n(matplotlib not installed — summary only. pip3 install matplotlib for {args.out})")
        return

    def series(rows):
        t0 = rows[0][0]
        return ([(ts - t0) / 60.0 for ts, _, _ in rows],
                [tot - sus for _, tot, sus in rows],
                [sus for _, _, sus in rows])

    fig, ax = plt.subplots(figsize=(11, 5.5))
    m, run, sus = series(csn_rows)
    ax.fill_between(m, 0, run, color="#d9534f", alpha=0.7, label="CSN running")
    ax.fill_between(m, run, [r+s for r, s in zip(run, sus, strict=True)], color="#5bc0de",
                    alpha=0.55, label="CSN suspended (~%.0f%%)" % (args.suspended_frac*100))
    if args.compare:
        sm, srun, _ = series(static_rows)
        ax.plot(sm, srun, color="#333", lw=2, ls="--", label="static running")
    elif args.static_nodes:
        ax.axhline(args.static_nodes, color="#333", lw=2, ls="--",
                   label=f"static {args.static_nodes} always-on")
    ax.set_xlabel("minutes into run")
    ax.set_ylabel("nodes")
    ax.set_title(f"Node footprint: CSN vs static — CSN = {ratio:.0f}% of static cost "
                 f"({100-ratio:.0f}% cheaper)")
    ax.legend(loc="upper right")
    ax.grid(True, alpha=0.3)
    fig.tight_layout()
    fig.savefig(args.out, dpi=130)
    print(f"\nsaved {args.out}")


if __name__ == "__main__":
    main()
