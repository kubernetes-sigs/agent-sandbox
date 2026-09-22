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
Unit tests for the gke-swap python-density telemetry parser.

parse_telemetry.process_telemetry() is a pure CSV -> JSON aggregation
transform that runs after a density sweep; these tests exercise it directly
against scratch CSV/JSON fixtures. No GKE cluster, Local SSD, or container
build is needed.
"""

import json

import pytest

from parse_telemetry import process_telemetry


def write_csv(path, rows):
    with open(path, "w", encoding="utf-8") as f:
        for row in rows:
            f.write(",".join(row) + "\n")


def write_json(path, data):
    with open(path, "w", encoding="utf-8") as f:
        json.dump(data, f)


def test_raises_when_csv_missing(tmp_path):
    json_path = tmp_path / "density.json"
    write_json(json_path, {})

    with pytest.raises(FileNotFoundError):
        process_telemetry(str(tmp_path / "missing.csv"), str(json_path))


def test_raises_when_json_missing(tmp_path):
    csv_path = tmp_path / "telemetry.csv"
    write_csv(csv_path, [["t", "node_memory_working_set_bytes", "1"]])

    with pytest.raises(FileNotFoundError):
        process_telemetry(str(csv_path), str(tmp_path / "missing.json"))


def test_computes_peaks_and_deltas(tmp_path):
    csv_path = tmp_path / "telemetry.csv"
    json_path = tmp_path / "density.json"
    write_csv(csv_path, [
        ["t0", "node_memory_working_set_bytes", str(1 * 1024**3)],
        ["t1", "node_memory_working_set_bytes", str(3 * 1024**3)],
        ["t0", "host_swap_used_bytes", "0"],
        ["t1", "host_swap_used_bytes", str(0.5 * 1024**3)],
        ["t0", "kubelet_memory", "0"],
        ["t1", "kubelet_memory", str(50 * 1024**2)],
        ["t0", "runtime_memory", "0"],
        ["t1", "runtime_memory", str(20 * 1024**2)],
        ["t0", "host_mem_psi_waiting_seconds_total", "1.0"],
        ["t1", "host_mem_psi_waiting_seconds_total", "1.25"],
        ["t0", "host_io_psi_waiting_seconds_total", "2.0"],
        ["t1", "host_io_psi_waiting_seconds_total", "2.5"],
        ["t0", "host_cpu_psi_waiting_seconds_total", "0.1"],
        ["t1", "host_cpu_psi_waiting_seconds_total", "0.4"],
    ])
    write_json(json_path, {"existing": "field"})

    process_telemetry(str(csv_path), str(json_path))

    data = json.loads(json_path.read_text())
    assert data["existing"] == "field"  # original content is preserved, not replaced
    peaks = data["node_telemetry_peaks"]
    assert peaks == {
        "peak_node_ram_gb": 3.0,
        "net_ram_added_gb": 2.0,
        "net_swap_added_gb": 0.5,
        "kubelet_memory_mb": 50.0,
        "containerd_memory_mb": 20.0,
        "mem_psi_seconds": 0.25,
        "io_psi_seconds": 0.5,
        "cpu_psi_seconds": 0.3,
    }


def test_skips_rows_with_too_few_columns(tmp_path):
    csv_path = tmp_path / "telemetry.csv"
    json_path = tmp_path / "density.json"
    write_csv(csv_path, [
        ["t0", "node_memory_working_set_bytes"],  # only 2 columns, must be skipped
        ["t1", "node_memory_working_set_bytes", str(2 * 1024**3)],
    ])
    write_json(json_path, {})

    process_telemetry(str(csv_path), str(json_path))

    peaks = json.loads(json_path.read_text())["node_telemetry_peaks"]
    assert peaks["peak_node_ram_gb"] == 2.0
    assert peaks["net_ram_added_gb"] == 0.0  # only one valid sample survives -> min == max


def test_skips_rows_with_non_numeric_value(tmp_path):
    csv_path = tmp_path / "telemetry.csv"
    json_path = tmp_path / "density.json"
    write_csv(csv_path, [
        ["t0", "node_memory_working_set_bytes", "not-a-number"],
        ["t1", "node_memory_working_set_bytes", str(1 * 1024**3)],
    ])
    write_json(json_path, {})

    process_telemetry(str(csv_path), str(json_path))

    peaks = json.loads(json_path.read_text())["node_telemetry_peaks"]
    assert peaks["peak_node_ram_gb"] == 1.0
    assert peaks["net_ram_added_gb"] == 0.0


def test_missing_metric_defaults_to_zero(tmp_path):
    csv_path = tmp_path / "telemetry.csv"
    json_path = tmp_path / "density.json"
    write_csv(csv_path, [["t0", "unrelated_metric", "5"]])
    write_json(json_path, {})

    process_telemetry(str(csv_path), str(json_path))

    peaks = json.loads(json_path.read_text())["node_telemetry_peaks"]
    assert peaks["peak_node_ram_gb"] == 0.0
    assert peaks["net_ram_added_gb"] == 0.0
    assert peaks["net_swap_added_gb"] == 0.0
