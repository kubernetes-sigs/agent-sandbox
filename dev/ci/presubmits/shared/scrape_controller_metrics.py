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

import argparse
import os
import re
import sys
import urllib.request


def parse_histogram(metric_name, data):
    """Parses Prometheus histogram buckets and computes interpolated percentiles.

    The controller histograms are labeled (e.g. launch_type, sandbox_template). This parser
    aggregates bucket counts across all label sets (equivalent to PromQL `sum(...) by (le)`).
    """
    value_re = r"([0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?)"
    pattern = re.compile(
        rf"^{re.escape(metric_name)}_bucket\{{[^}}]*\ble=\"([^\"]+)\"[^}}]*\}}\s+{value_re}",
        re.MULTILINE,
    )
    bucket_counts = {}
    total = 0.0
    for match in pattern.finditer(data):
        le_str, count_str = match.groups()
        c = float(count_str)
        if le_str == "+Inf":
            total += c
            continue
        le = float(le_str)
        bucket_counts[le] = bucket_counts.get(le, 0.0) + c

    buckets = sorted(bucket_counts.items())
    if total == 0.0 and buckets:
        total = buckets[-1][1]
    if total == 0.0:
        return None

    def get_percentile(p):
        target = p * total
        prev_le, prev_count = 0.0, 0.0
        for le, count in buckets:
            if count >= target:
                if count == prev_count:
                    return le
                # Linear interpolation between adjacent histogram buckets
                fraction = (target - prev_count) / (count - prev_count)
                return prev_le + fraction * (le - prev_le)
            prev_le, prev_count = le, count
        return float("inf")

    return get_percentile(0.50), get_percentile(0.90), get_percentile(0.99), int(total)


def main():
    parser = argparse.ArgumentParser(description="Scrape and validate Agent Sandbox controller metrics.")
    parser.add_argument(
        "--metrics-url",
        default=os.environ.get("CONTROLLER_METRICS_URL", "http://127.0.0.1:8081/metrics"),
        help="Prometheus metrics endpoint URL of the controller",
    )
    parser.add_argument(
        "--threshold-50",
        type=float,
        default=float(os.environ.get("CL2_THRESHOLD_50_MS", 300)),
        help="Target P50 latency threshold in ms for claim adoption",
    )
    parser.add_argument(
        "--threshold-90",
        type=float,
        default=float(os.environ.get("CL2_THRESHOLD_90_MS", 300)),
        help="Target P90 latency threshold in ms for claim adoption",
    )
    parser.add_argument(
        "--threshold-99",
        type=float,
        default=float(os.environ.get("CL2_THRESHOLD_99_MS", 500)),
        help="Target P99 latency threshold in ms for claim adoption",
    )
    args = parser.parse_args()

    all_passed = True

    try:
        raw = urllib.request.urlopen(args.metrics_url, timeout=5).read().decode("utf-8")

        # 1. Controller Claim Adoption Latency (Gated SLO)
        res = parse_histogram("agent_sandbox_claim_controller_startup_latency_ms", raw)
        print("  • Controller Claim Adoption Latency (ms):")
        if res:
            p50, p90, p99, count = res
            v50 = " [VIOLATION]" if p50 > args.threshold_50 else ""
            v90 = " [VIOLATION]" if p90 > args.threshold_90 else ""
            v99 = " [VIOLATION]" if p99 > args.threshold_99 else ""
            print(f"      Sample Size: {count}")
            print(f"      P50: ~{p50:.1f} ms (Target: <= {args.threshold_50:.1f} ms){v50}")
            print(f"      P90: ~{p90:.1f} ms (Target: <= {args.threshold_90:.1f} ms){v90}")
            print(f"      P99: ~{p99:.1f} ms (Target: <= {args.threshold_99:.1f} ms){v99}")
            if p50 > args.threshold_50 or p90 > args.threshold_90 or p99 > args.threshold_99:
                print("      Status: [FAILED] ❌ (Latency exceeded threshold)")
                all_passed = False
            else:
                print("      Status: [PASSED] ✅")
        else:
            print("      Status: [FAILED] ❌ (No metrics recorded)")
            all_passed = False

        print("")
        # 2. Sandbox Pod Provisioning Latency (Informational Telemetry)
        res_prov = parse_histogram("agent_sandbox_creation_latency_ms", raw)
        if res_prov:
            p50, p90, p99, count = res_prov
            print(f"  • Sandbox Pod Provisioning Latency (ms) (Sample Size: {count}):")
            print(f"      P50: ~{p50:.1f} ms | P90: ~{p90:.1f} ms | P99: ~{p99:.1f} ms")
        else:
            print("  • Sandbox Pod Provisioning Latency (ms): No metrics recorded")

    except Exception as e:
        print(f"  Warning: Could not fetch metrics from controller: {e}")
        all_passed = False

    if not all_passed:
        sys.exit(1)


if __name__ == "__main__":
    main()
