#!/usr/bin/env python3
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

"""Plot degradation curves from sandbox-capacity-cliff-test run artifacts.

Usage:
    python3 plot_capacity_results.py tmp/<RUN_ID> [tmp/<OTHER_RUN_ID> ...] -o out.png

Reads, per run directory:
  - junit.xml                      -> per-step convergence wall time ("Wait for N Sandboxes")
  - GenericPrometheusQuery*.json   -> per-step controller RSS, workqueue depth/p99

All run directories are overlaid on every panel (convergence from junit,
metric panels from the per-step JSONs), so before/after comparisons read
consistently across the whole figure. Requires matplotlib.
"""
import argparse
import glob
import json
import os
import re
import sys
import xml.etree.ElementTree as ET

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

# Reference dataviz palette (light mode)
SURFACE, INK, INK2, MUTED = "#fcfcfb", "#0b0b0b", "#52514e", "#898781"
GRID, BASELINE = "#e1e0d9", "#c3c2b7"
SERIES = ["#2a78d6", "#1baf7a", "#eda100", "#008300"]  # fixed order, never cycled


def load_run(run_dir):
    counts, conv, metrics = [], {}, {}
    junit = os.path.join(run_dir, "junit.xml")
    if os.path.exists(junit):
        with open(junit) as fh:
            text = fh.read()
        # Tolerate a license comment prepended above the XML declaration
        # (repo boilerplate tooling does this to artifacts left in the tree),
        # which is invalid XML if parsed as-is.
        start = text.find("<?xml")
        if start == -1:
            start = text.find("<testsuite")
        root = ET.fromstring(text[max(start, 0):])
        suite = root if root.tag == "testsuite" else root.find("testsuite")
        for tc in suite.iter("testcase"):
            m = re.search(r"Wait for (\d+) Sandboxes", tc.get("name", ""))
            if m:
                conv[int(m.group(1))] = float(tc.get("time"))
    for f in glob.glob(os.path.join(run_dir, "GenericPrometheusQuery*.json")):
        m = re.search(r"\((\d+) sandboxes\)", f)
        if not m:
            continue
        count = int(m.group(1))
        with open(f) as fh:
            data = json.load(fh)
        items = data.get("dataItems") or [{}]
        metrics[count] = items[0].get("data", {})
    counts = sorted(set(conv) | set(metrics))
    return counts, conv, metrics


def style(ax, title, subtitle):
    ax.set_facecolor(SURFACE)
    for side in ("top", "right", "left"):
        ax.spines[side].set_visible(False)
    ax.spines["bottom"].set_color(BASELINE)
    ax.tick_params(colors=MUTED, labelsize=9, length=0)
    ax.grid(axis="y", color=GRID, linewidth=0.75)
    ax.set_axisbelow(True)
    ax.set_title(title, loc="left", fontsize=11.5, color=INK, fontweight="bold", pad=16)
    ax.text(0, 1.045, subtitle, transform=ax.transAxes, fontsize=9, color=INK2)


def kfmt(v):
    return f"{v // 1000}k" if v >= 1000 else str(v)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_dirs", nargs="+")
    ap.add_argument("-o", "--out", default="capacity-degradation.png")
    ap.add_argument("--labels", nargs="*", help="one label per run dir for the convergence panel")
    args = ap.parse_args()
    if args.labels and len(args.labels) != len(args.run_dirs):
        ap.error(f"--labels has {len(args.labels)} entries but {len(args.run_dirs)} run_dirs were given")

    runs = [(d, *load_run(d)) for d in args.run_dirs]
    labels = args.labels or [os.path.basename(os.path.normpath(d)) for d in args.run_dirs]
    primary_dir, counts, conv, metrics = runs[0]
    if not counts:
        sys.exit(f"no junit.xml or metrics JSONs found in {primary_dir}")

    fig, axes = plt.subplots(2, 2, figsize=(12.8, 8.6), dpi=150)
    fig.patch.set_facecolor(SURFACE)
    fig.subplots_adjust(hspace=0.52, wspace=0.24, left=0.07, right=0.97, top=0.83, bottom=0.08)

    def add_legend(ax):
        if len(runs) > 1:
            ax.legend(loc="upper left", frameon=False, fontsize=8.5, labelcolor=INK2, handlelength=1.6)

    # Panel 1: convergence (all runs, from junit)
    ax = axes[0][0]
    style(ax, "Time to converge each batch", "seconds until all sandboxes Ready")
    for i, (d, cts, cv, _) in enumerate(runs):
        xs = sorted(cv)
        ax.plot(xs, [cv[x] for x in xs], color=SERIES[i % len(SERIES)], linewidth=2,
                marker="o", markersize=5, label=labels[i])
    add_legend(ax)

    panels = [
        (axes[0][1], "SandboxWorkqueueSecondsP99", "Sandbox workqueue p99 queue time", "seconds, log scale", True, 1),
        (axes[1][0], "SandboxWorkqueueDepthMax", "Sandbox workqueue max depth per step", "items", False, 1),
        (axes[1][1], "ControllerResidentMemoryBytes", "Controller resident memory", "GiB", False, 1 << 30),
    ]
    for ax, metric, title, unit, logy, div in panels:
        style(ax, title, unit)
        any_data = False
        for i, (d, cts, cv, mets) in enumerate(runs):
            xs = [c for c in sorted(mets) if mets[c].get(metric) is not None]
            if not xs:
                continue
            any_data = True
            ax.plot(xs, [mets[c][metric] / div for c in xs], color=SERIES[i % len(SERIES)],
                    linewidth=2, marker="o", markersize=5, label=labels[i])
        if not any_data:
            ax.text(0.5, 0.5, "no data", transform=ax.transAxes, color=MUTED, ha="center")
            continue
        if logy:
            ax.set_yscale("log")
        add_legend(ax)

    for ax in axes.flat:
        ticks = [c for c in counts[1::2]]
        ax.set_xticks(ticks)
        ax.set_xticklabels([kfmt(t) for t in ticks])

    fig.suptitle("Sandbox capacity degradation", x=0.07, y=0.98, ha="left",
                 fontsize=13.5, color=INK, fontweight="bold")
    fig.text(0.07, 0.945, " · ".join(labels), fontsize=9.5, color=INK2)
    fig.savefig(args.out, facecolor=SURFACE)
    print("wrote", args.out)


if __name__ == "__main__":
    main()
