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

"""Parses cAdvisor telemetry CSV profiles and attaches node metrics to density JSON results."""

import csv
import json
import os
import sys


def process_telemetry(csv_path: str, json_path: str) -> None:
    if not os.path.exists(csv_path):
        raise FileNotFoundError(f"Telemetry CSV file not found: {csv_path}")
    if not os.path.exists(json_path):
        raise FileNotFoundError(f"Density metrics JSON file not found: {json_path}")

    metrics_min = {}
    metrics_max = {}
    with open(csv_path, "r", encoding="utf-8") as f:
        reader = csv.reader(f)
        for row in reader:
            if len(row) < 3:
                continue
            metric_name = row[1].strip()
            try:
                val = float(row[2].strip())
            except ValueError:
                continue
            if metric_name not in metrics_min:
                metrics_min[metric_name] = val
                metrics_max[metric_name] = val
            else:
                if val < metrics_min[metric_name]:
                    metrics_min[metric_name] = val
                if val > metrics_max[metric_name]:
                    metrics_max[metric_name] = val

    ram_max = round(
        metrics_max.get("node_memory_working_set_bytes", 0.0) / (1024**3), 2
    )
    ram_delta = round(
        (
            metrics_max.get("node_memory_working_set_bytes", 0.0)
            - metrics_min.get("node_memory_working_set_bytes", 0.0)
        )
        / (1024**3),
        2,
    )
    swap_delta = round(
        (
            metrics_max.get("host_swap_used_bytes", 0.0)
            - metrics_min.get("host_swap_used_bytes", 0.0)
        )
        / (1024**3),
        2,
    )

    node_telemetry = {
        "peak_node_ram_gb": ram_max,
        "net_ram_added_gb": max(ram_delta, 0.0),
        "net_swap_added_gb": max(swap_delta, 0.0),
        "kubelet_memory_mb": round(
            metrics_max.get("kubelet_memory", 0.0) / (1024**2), 1
        ),
        "containerd_memory_mb": round(
            metrics_max.get("runtime_memory", 0.0) / (1024**2), 1
        ),
        "mem_psi_seconds": round(
            metrics_max.get("host_mem_psi_waiting_seconds_total", 0.0)
            - metrics_min.get("host_mem_psi_waiting_seconds_total", 0.0),
            4,
        ),
        "io_psi_seconds": round(
            metrics_max.get("host_io_psi_waiting_seconds_total", 0.0)
            - metrics_min.get("host_io_psi_waiting_seconds_total", 0.0),
            4,
        ),
        "cpu_psi_seconds": round(
            metrics_max.get("host_cpu_psi_waiting_seconds_total", 0.0)
            - metrics_min.get("host_cpu_psi_waiting_seconds_total", 0.0),
            4,
        ),
    }

    with open(json_path, "r", encoding="utf-8") as f:
        data = json.load(f)

    data["node_telemetry_peaks"] = node_telemetry

    with open(json_path, "w", encoding="utf-8") as f:
        json.dump(data, f, indent=2)


if __name__ == "__main__":
    if len(sys.argv) < 3:
        print(f"Usage: {sys.argv[0]} <resource_profile.csv> <density_metrics.json>", file=sys.stderr)
        sys.exit(1)
    process_telemetry(sys.argv[1], sys.argv[2])
