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

"""Keeps the bundled Grafana dashboard aligned with the generated metric catalog."""

import json
import re
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
DASHBOARD_PATH = REPO_ROOT / "helm" / "dashboards" / "agent-sandbox-controller.json"
METRICS_DOC_PATH = REPO_ROOT / "docs" / "metrics.md"

METRIC_ROW_RE = re.compile(r"^\| `(?P<name>agent_sandbox[a-z0-9_]*)` \|", re.MULTILINE)
METRIC_REF_RE = re.compile(r"\bagent_sandbox[a-z0-9_]*\b")
HISTOGRAM_SUFFIXES = ("_bucket", "_sum", "_count")
CORE_METRICS = {
    "agent_sandbox_build_info",
    "agent_sandbox_claim_controller_startup_latency_ms",
    "agent_sandbox_claim_creation_total",
    "agent_sandbox_creation_latency_ms",
    "agent_sandbox_expired_total",
    "agent_sandbox_finished_total",
    "agent_sandbox_resume_latency_ms",
    "agent_sandbox_resume_total",
    "agent_sandbox_suspend_latency_ms",
    "agent_sandbox_suspend_total",
    "agent_sandbox_warmpool_size",
    "agent_sandboxes",
}


def _documented_metrics():
    return set(METRIC_ROW_RE.findall(METRICS_DOC_PATH.read_text()))


def _dashboard():
    return json.loads(DASHBOARD_PATH.read_text())


def _query_expressions(value):
    if isinstance(value, dict):
        for key, nested in value.items():
            if key == "expr" and isinstance(nested, str):
                yield nested
            else:
                yield from _query_expressions(nested)
    elif isinstance(value, list):
        for nested in value:
            yield from _query_expressions(nested)


def _normalized_metric(name, documented):
    if name in documented:
        return name
    for suffix in HISTOGRAM_SUFFIXES:
        if name.endswith(suffix) and name[: -len(suffix)] in documented:
            return name[: -len(suffix)]
    return name


def test_dashboard_only_queries_documented_metric_families():
    documented = _documented_metrics()
    referenced = {
        _normalized_metric(match, documented)
        for expr in _query_expressions(_dashboard())
        for match in METRIC_REF_RE.findall(expr)
    }
    assert referenced
    assert referenced <= documented, f"Undocumented dashboard metrics: {sorted(referenced - documented)}"


def test_dashboard_covers_the_core_controller_metric_rows():
    documented = _documented_metrics()
    referenced = {
        _normalized_metric(match, documented)
        for expr in _query_expressions(_dashboard())
        for match in METRIC_REF_RE.findall(expr)
    }
    assert CORE_METRICS <= referenced
