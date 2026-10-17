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

import json

import duckdb
import pytest

from generate_report import containerd_operations_by_phase


@pytest.fixture
def conn():
    with duckdb.connect() as conn:
        conn.execute("CREATE TABLE phases (name VARCHAR, start_time TIMESTAMP, end_time TIMESTAMP)")
        conn.execute("""INSERT INTO phases VALUES
            ('first', '2026-10-17 00:00:00', '2026-10-17 00:00:30'),
            ('second', '2026-10-17 00:00:30', '2026-10-17 00:01:00')""")
        yield conn


def timer_samples(metric, node, observations, labels=None, source="containerd"):
    return [
        {"ts": f"2026-10-17T00:00:{second:02d}Z", "source": source,
         "instance": node, "metric": f"{metric}_{suffix}", "labels": labels or {}, "value": value}
        for second, count, duration in observations
        for suffix, value in (("count", count), ("sum", duration))
    ]


def write_metrics(tmp_path, samples):
    path = tmp_path / "metrics.jsonl"
    path.write_text("".join(json.dumps(sample) + "\n" for sample in samples))
    return str(path)


def test_containerd_operations_keep_nodes_runtimes_and_cni_operations_separate(conn, tmp_path):
    runtime = "containerd_cri_sandbox_runtime_create_seconds"
    network = "containerd_cri_sandbox_create_network_seconds"
    cni = "containerd_cri_network_plugin_operations_duration_seconds_seconds"
    samples = timer_samples(runtime, "node-a", [(0, 10, 20), (20, 14, 28), (40, 20, 46)], {"runtime": "runc"})
    samples += timer_samples(runtime, "node-a", [(0, 2, 10), (20, 3, 15)], {"runtime": "runsc"})
    samples += timer_samples(runtime, "node-b", [(0, 1, 2), (20, 3, 4)], {"runtime": "runc"})
    samples += timer_samples(network, "node-a", [(0, 10, 5), (20, 14, 7)])
    samples += timer_samples(cni, "node-a", [(0, 10, 2), (20, 14, 3)], {"operation_type": "set_up_pod"})
    samples += timer_samples(cni, "node-a", [(0, 5, 1), (20, 7, 1.2)], {"operation_type": "tear_down_pod"})
    samples += timer_samples(runtime, "node-a", [(0, 100, 100), (20, 200, 300)], {"runtime": "runc"}, source="other")
    rows = containerd_operations_by_phase(conn, write_metrics(tmp_path, samples))
    actual = {
        (r["phase_name"], r["instance"], r["operation"], r["runtime"]): (r["count_delta"], r["avg_latency_ms"])
        for r in rows
    }
    expected = {
        ("first", "node-a", "Sandbox creation (includes network)", "runc"): (4, 2000),
        ("first", "node-a", "Sandbox creation (includes network)", "runsc"): (1, 5000),
        ("first", "node-b", "Sandbox creation (includes network)", "runc"): (2, 1000),
        ("first", "node-a", "Network setup (includes CNI)", ""): (4, 500),
        ("first", "node-a", "CNI set_up_pod", ""): (4, 250),
        ("first", "node-a", "CNI tear_down_pod", ""): (2, 100),
        ("second", "node-a", "Sandbox creation (includes network)", "runc"): (6, 3000),
    }
    assert actual.keys() == expected.keys()
    for key, value in expected.items():
        assert actual[key] == pytest.approx(value)


def test_containerd_operations_drop_resets_and_zero_counts(conn, tmp_path):
    metric = "containerd_cri_sandbox_create_network_seconds"
    samples = timer_samples(metric, "node-a", [(0, 10, 20), (20, 1, 1), (40, 3, 5), (50, 3, 5)])
    rows = containerd_operations_by_phase(conn, write_metrics(tmp_path, samples))
    assert rows == [{"phase_name": "second", "instance": "node-a",
                     "operation": "Network setup (includes CNI)", "runtime": "",
                     "count_delta": 2, "avg_latency_ms": 2000}]


def test_containerd_operations_allow_older_reports_and_single_scrapes(conn, tmp_path):
    samples = timer_samples("kubelet_runtime_operations_duration_seconds", "node-a", [(0, 1, 1), (20, 2, 2)], source="kubelet")
    path = write_metrics(tmp_path, samples)
    assert containerd_operations_by_phase(conn, path) == []
    samples += timer_samples("containerd_cri_sandbox_runtime_create_seconds", "node-a", [(20, 3, 9)], {"runtime": "runc"})
    assert containerd_operations_by_phase(conn, write_metrics(tmp_path, samples)) == []
