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
import gzip
import json
import os
import shutil
import sys
from datetime import datetime, timedelta
from pathlib import Path
import duckdb
from jinja2 import Environment, FileSystemLoader, select_autoescape

# Every cumulative-counter query shares one shape, and all of it is easy to
# get subtly wrong: deltas must be lag()-diffed per metric stream (source +
# instance + every label -- mixing streams corrupts deltas), counter resets
# must be dropped, and samples attributed to phases with half-open windows.
# These helpers own that shape; queries supply only metric names, labels,
# and grouping.

def _counter_deltas_cte(metrics_path, metrics, labels, where):
    """Shared CTE prefix computing per-stream deltas of cumulative counters."""
    label_selects = "".join(
        f",\n                COALESCE(CAST(labels->>'{prom}' AS VARCHAR), '') as {col}"
        for col, prom in labels.items()
    )
    metric_list = ", ".join(f"'{m}'" for m in metrics)
    partition = ", ".join(["source", "instance", *labels, "metric"])
    where_clause = f"\n            WHERE {where}" if where else ""
    return f"""
        WITH raw AS (
            SELECT
                CAST(ts AS TIMESTAMP) as ts,
                source,
                CAST(instance AS VARCHAR) as instance,
                metric,
                value{label_selects}
            FROM read_json_auto('{metrics_path}')
            WHERE metric IN ({metric_list})
        ),
        diffs AS (
            SELECT
                *,
                value - lag(value) OVER w as delta,
                -- Midpoint of the delta window (previous sample -> this
                -- sample): used to attribute the window to the phase it
                -- mostly overlaps, so increments accumulated late in phase
                -- A are not booked to phase B just because the scrape
                -- landed after the boundary.
                lag(ts) OVER w + (ts - lag(ts) OVER w) / 2 as window_mid
            FROM raw{where_clause}
            WINDOW w AS (PARTITION BY {partition} ORDER BY ts)
        )"""


def metrics_by_phase(conn, metrics_path, metrics, labels=None, group_by=None, where=None):
    """Sums per-scrape counter deltas per phase.

    metrics: cumulative counter names; the result has one summed column per
        entry, in the given order.
    labels: {sql_column: prometheus_label} to extract; every extracted label
        partitions the delta computation.
    group_by: columns to group by (defaults to all extracted labels; may
        also name the built-in source / instance columns).
    where: optional SQL filter over the extracted columns.

    Returns rows of (phase_name, *group_by, *summed_metrics).
    """
    labels = labels or {}
    group_by = list(labels) if group_by is None else group_by
    sums = ",\n            ".join(
        f"SUM(CASE WHEN metric = '{m}' THEN delta ELSE 0 END)"
        for m in metrics
    )
    group_cols = ", ".join(["phase_name", *group_by])
    sql = _counter_deltas_cte(metrics_path, metrics, labels, where) + f""",
        with_phase AS (
            SELECT p.name as phase_name, d.*
            FROM diffs d
            JOIN phases p ON d.window_mid >= p.start_time AND d.window_mid < p.end_time
            WHERE d.delta >= 0
        )
        SELECT
            {group_cols},
            {sums}
        FROM with_phase
        GROUP BY {group_cols}
        ORDER BY {group_cols}
    """
    return conn.execute(sql).fetchall()


def metrics_timeseries(conn, metrics_path, metrics, labels=None, group_by=None, where=None, interval="15 seconds"):
    """Sums per-scrape counter deltas into time buckets.

    Same contract as metrics_by_phase, but bucketed by time instead of
    phase. Returns rows of (ts_string, *group_by, *summed_metrics).
    """
    labels = labels or {}
    group_by = list(labels) if group_by is None else group_by
    select_groups = "".join(f",\n                {c}" for c in group_by)
    sums = ",\n                ".join(
        f"SUM(CASE WHEN metric = '{m}' THEN delta ELSE 0 END)"
        for m in metrics
    )
    order_cols = ", ".join(["bucket_time", *group_by])
    sql = _counter_deltas_cte(metrics_path, metrics, labels, where) + f""",
        binned AS (
            SELECT
                time_bucket(INTERVAL '{interval}', ts) as bucket_time{select_groups},
                {sums}
            FROM diffs
            WHERE delta >= 0
            GROUP BY {order_cols}
        )
        SELECT strftime(bucket_time, '%Y-%m-%dT%H:%M:%SZ') as ts, * EXCLUDE (bucket_time)
        FROM binned
        ORDER BY {order_cols}
    """
    return conn.execute(sql).fetchall()


def main():
    parser = argparse.ArgumentParser(description="Generate stress test bottleneck report.")
    parser.add_argument("--input-dir", required=True, help="Directory containing stress test outputs (summary.json, metrics.jsonl)")
    parser.add_argument("--output-dir", required=True, help="Directory to write static HTML reports")
    args = parser.parse_args()

    input_dir = Path(args.input_dir)
    output_dir = Path(args.output_dir)

    summary_file = input_dir / "summary.json"
    metrics_file = input_dir / "metrics.jsonl"

    if not summary_file.exists():
        summary_file = input_dir / "summary.json.gz"

    if not summary_file.exists():
        print(f"Error: summary.json or summary.json.gz not found in {input_dir}", file=sys.stderr)
        sys.exit(1)

    if not metrics_file.exists():
        metrics_file = input_dir / "metrics.jsonl.gz"

    if not metrics_file.exists():
        print(f"Error: metrics.jsonl or metrics.jsonl.gz not found in {input_dir}", file=sys.stderr)
        sys.exit(1)

    sandboxes_file = input_dir / "sandboxes.jsonl"
    if not sandboxes_file.exists():
        sandboxes_file = input_dir / "sandboxes.jsonl.gz"

    watch_file = input_dir / "watch.jsonl"
    if not watch_file.exists():
        watch_file = input_dir / "watch.jsonl.gz"

    # 1. Load summary data
    opener = gzip.open if summary_file.suffix == '.gz' else open
    with opener(summary_file, 'rt') as f:
        summary = json.load(f)

    # Keep timestamps as naive UTC: DuckDB converts tz-aware values to local
    # time when storing them in a TIMESTAMP column, which would mis-align the
    # phases table with the naive-UTC timestamps parsed from the metrics files.
    start_time = datetime.fromisoformat(summary['startTime'].replace('Z', '+00:00')).replace(tzinfo=None)
    end_time = datetime.fromisoformat(summary['endTime'].replace('Z', '+00:00')).replace(tzinfo=None)
    run_duration = int((end_time - start_time).total_seconds())

    # Map phases to time ranges
    phases = []
    total_created = 0
    probe_latency_ms = 0.0

    for phase in summary['phases']:
        p_start = start_time + timedelta(seconds=phase['startOffsetSeconds'])
        p_end = p_start + timedelta(seconds=phase['durationSeconds'])
        phases.append((phase['name'], p_start, p_end))
        total_created += phase.get('created', 0)
        if phase['name'] == 'probe':
            probe_latency_ms = phase['latency']['endToEndReady']['meanMs']

    js_phases = []
    for phase in summary.get('phases', []):
        p_start = start_time + timedelta(seconds=phase['startOffsetSeconds'])
        p_end = p_start + timedelta(seconds=phase['durationSeconds'])
        js_phases.append({
            "name": phase["name"],
            "start_sec": phase["startOffsetSeconds"],
            "end_sec": phase["startOffsetSeconds"] + phase["durationSeconds"],
            "start_ts": p_start.strftime('%Y-%m-%dT%H:%M:%SZ'),
            "end_ts": p_end.strftime('%Y-%m-%dT%H:%M:%SZ')
        })

    start_time_iso = start_time.strftime('%Y-%m-%d %H:%M:%S.%f')
    end_time_iso = end_time.strftime('%Y-%m-%d %H:%M:%S.%f')

    # 2. Set up DuckDB and populate phases table
    conn = duckdb.connect()
    conn.execute("CREATE TABLE phases (name VARCHAR, start_time TIMESTAMP, end_time TIMESTAMP)")
    for name, p_start, p_end in phases:
        conn.execute("INSERT INTO phases VALUES (?, ?, ?)", (name, p_start, p_end))

    # Escape filename for DuckDB
    metrics_path_str = str(metrics_file).replace("'", "''")

    # 3. Execute queries
    print("Querying CRI operations by phase...")
    cri_ops_raw = metrics_by_phase(
        conn, metrics_path_str,
        metrics=["kubelet_runtime_operations_duration_seconds_count",
                 "kubelet_runtime_operations_duration_seconds_sum"],
        labels={"operation_type": "operation_type"})
    cri_ops_raw = [r for r in cri_ops_raw if r[2] > 0]
    cri_ops_raw.sort(key=lambda r: (r[0], -r[2]))

    cri_ops = []
    for row in cri_ops_raw:
        phase_name, op, count_delta, sum_delta = row
        avg_latency_ms = (sum_delta / count_delta) * 1000 if count_delta > 0 else 0
        cri_ops.append({
            "phase_name": phase_name,
            "operation_type": op,
            "count_delta": int(count_delta),
            "avg_latency_ms": avg_latency_ms
        })

    print("Querying CRI timeseries...")
    cri_ts_raw = metrics_timeseries(
        conn, metrics_path_str,
        metrics=["kubelet_runtime_operations_duration_seconds_count",
                 "kubelet_runtime_operations_duration_seconds_sum"],
        labels={"operation_type": "operation_type"},
        group_by=["instance"],
        where="operation_type = 'run_podsandbox'")

    cri_chart_data = [
        {"ts": row[0], "instance": row[1], "count": int(row[2]),
         "avg_latency_s": row[3] / row[2] if row[2] > 0 else 0.0}
        for row in cri_ts_raw
    ]

    print("Querying controller performance by phase...")
    controller_ops_raw = metrics_by_phase(
        conn, metrics_path_str,
        metrics=["controller_runtime_reconcile_total",
                 "controller_runtime_reconcile_errors_total",
                 "controller_runtime_reconcile_time_seconds_sum",
                 "controller_runtime_reconcile_time_seconds_count"],
        labels={"controller": "controller", "result": "result"},
        group_by=["controller"])

    controller_ops = []
    for row in controller_ops_raw:
        phase_name, controller, total_reconciles, total_errors, sum_time, count_time = row
        avg_reconcile_ms = (sum_time / count_time) * 1000 if count_time > 0 else 0
        error_rate = (total_errors / total_reconciles) * 100 if total_reconciles > 0 else 0
        controller_ops.append({
            "phase_name": phase_name,
            "controller": controller,
            "total_reconciles": int(total_reconciles),
            "total_errors": int(total_errors),
            "error_rate": error_rate,
            "avg_reconcile_ms": avg_reconcile_ms
        })

    print("Querying controller timeseries...")
    controller_ts_raw = metrics_timeseries(
        conn, metrics_path_str,
        metrics=["controller_runtime_reconcile_total"],
        labels={"controller": "controller", "result": "result"},
        group_by=["controller"])

    controller_chart_data = [
        {"ts": row[0], "controller": row[1], "reconcile_rate": row[2] / 15.0}
        for row in controller_ts_raw
    ]

    print("Querying apiserver operations by phase...")
    apiserver_ops_raw = metrics_by_phase(
        conn, metrics_path_str,
        metrics=["apiserver_request_duration_seconds_count",
                 "apiserver_request_duration_seconds_sum"],
        labels={"resource": "resource", "subresource": "subresource", "verb": "verb",
                "group_label": "group", "version": "version", "scope": "scope", "dry_run": "dry_run"},
        group_by=["resource", "verb"])
    apiserver_ops_raw = [r for r in apiserver_ops_raw if r[3] > 0]
    apiserver_ops_raw.sort(key=lambda r: (r[0], -r[3]))

    apiserver_ops = []
    for row in apiserver_ops_raw:
        phase_name, resource, verb, count_delta, sum_delta = row
        avg_latency_ms = (sum_delta / count_delta) * 1000 if count_delta > 0 else 0
        apiserver_ops.append({
            "phase_name": phase_name,
            "resource": resource,
            "verb": verb,
            "count_delta": int(count_delta),
            "avg_latency_ms": avg_latency_ms
        })

    print("Querying apiserver timeseries...")
    apiserver_ts_raw = metrics_timeseries(
        conn, metrics_path_str,
        metrics=["apiserver_request_duration_seconds_count"],
        labels={"resource": "resource", "subresource": "subresource", "verb": "verb",
                "group_label": "group", "version": "version", "scope": "scope", "dry_run": "dry_run"},
        group_by=["resource", "verb"])

    apiserver_chart_data = [
        {"ts": row[0], "resource": row[1], "verb": row[2], "request_rate": row[3] / 15.0}
        for row in apiserver_ts_raw
    ]

    print("Querying etcd operations by phase...")
    etcd_ops_raw = metrics_by_phase(
        conn, metrics_path_str,
        metrics=["etcd_request_duration_seconds_count",
                 "etcd_request_duration_seconds_sum"],
        labels={"resource": "resource", "operation": "operation", "group_label": "group"},
        group_by=["resource", "operation"])
    etcd_ops_raw = [r for r in etcd_ops_raw if r[3] > 0]
    etcd_ops_raw.sort(key=lambda r: (r[0], -r[3]))

    etcd_ops = []
    for row in etcd_ops_raw:
        phase_name, resource, operation, count_delta, sum_delta = row
        avg_latency_ms = (sum_delta / count_delta) * 1000 if count_delta > 0 else 0
        etcd_ops.append({
            "phase_name": phase_name,
            "resource": resource,
            "operation": operation,
            "count_delta": int(count_delta),
            "avg_latency_ms": avg_latency_ms
        })

    print("Querying etcd timeseries...")
    etcd_ts_raw = metrics_timeseries(
        conn, metrics_path_str,
        metrics=["etcd_request_duration_seconds_count"],
        labels={"resource": "resource", "operation": "operation", "group_label": "group"},
        group_by=["resource", "operation"])

    etcd_chart_data = [
        {"ts": row[0], "resource": row[1], "operation": row[2], "request_rate": row[3] / 15.0}
        for row in etcd_ts_raw
    ]

    # Cilium agent metrics (present when the cluster runs Cilium with
    # enablePrometheusMetrics). The CNI plugin calls the agent's REST API
    # synchronously during CNI ADD/DEL, so agent API latency is pod sandbox
    # creation latency. Cilium rate-limits endpoint-create requests
    # (api-rate-limit, default 0.5/s auto-adjusted), and that limiter's wait
    # time is where launch throughput ceilings show up.
    print("Querying cilium agent API latency by phase...")
    cilium_api_raw = metrics_by_phase(
        conn, metrics_path_str,
        metrics=["cilium_agent_api_process_time_seconds_count",
                 "cilium_agent_api_process_time_seconds_sum"],
        labels={"method": "method", "path": "path", "return_code": "return_code"},
        group_by=["method", "path"])
    cilium_api_raw = [r for r in cilium_api_raw if r[3] > 0]
    cilium_api_raw.sort(key=lambda r: (r[0], -r[4]))

    cilium_api_ops = []
    for row in cilium_api_raw:
        phase_name, method, path, count_delta, sum_delta = row
        avg_latency_ms = (sum_delta / count_delta) * 1000 if count_delta > 0 else 0
        cilium_api_ops.append({
            "phase_name": phase_name,
            "method": method,
            "path": path,
            "count_delta": int(count_delta),
            "avg_latency_ms": avg_latency_ms
        })

    print("Querying cilium endpoint-create limiter by phase...")
    # These are gauges (running mean / max since agent start), so per-phase
    # values are scrape averages: good for spotting sustained queueing,
    # not exact per-phase distributions.
    cilium_limiter_raw = conn.execute(f"""
        WITH raw AS (
            SELECT
                CAST(ts AS TIMESTAMP) as ts,
                CAST(instance AS VARCHAR) as instance,
                metric,
                CAST(labels->>'value' AS VARCHAR) as kind,
                value
            FROM read_json_auto('{metrics_path_str}')
            WHERE CAST(labels->>'api_call' AS VARCHAR) = 'endpoint-create'
              AND metric IN ('cilium_api_limiter_wait_duration_seconds',
                             'cilium_api_limiter_rate_limit',
                             'cilium_api_limiter_requests_in_flight',
                             'cilium_api_limiter_processing_duration_seconds')
        )
        SELECT
            p.name as phase_name,
            AVG(CASE WHEN metric = 'cilium_api_limiter_wait_duration_seconds' AND kind = 'mean' THEN value END) as wait_mean_s,
            MAX(CASE WHEN metric = 'cilium_api_limiter_wait_duration_seconds' AND kind = 'max' THEN value END) as wait_max_s,
            AVG(CASE WHEN metric = 'cilium_api_limiter_rate_limit' AND kind = 'limit' THEN value END) as rate_limit,
            AVG(CASE WHEN metric = 'cilium_api_limiter_requests_in_flight' AND kind = 'in-flight' THEN value END) as in_flight,
            AVG(CASE WHEN metric = 'cilium_api_limiter_processing_duration_seconds' AND kind = 'mean' THEN value END) as processing_mean_s
        FROM raw r
        JOIN phases p ON r.ts >= p.start_time AND r.ts < p.end_time
        GROUP BY p.name
        ORDER BY MIN(p.start_time)
    """).fetchall()

    cilium_limiter_summary = []
    for row in cilium_limiter_raw:
        cilium_limiter_summary.append({
            "phase_name": row[0],
            "wait_mean_s": row[1] or 0.0,
            "wait_max_s": row[2] or 0.0,
            "rate_limit": row[3] or 0.0,
            "in_flight": row[4] or 0.0,
            "processing_mean_s": row[5] or 0.0
        })

    print("Querying cilium limiter timeseries...")
    cilium_limiter_ts_raw = conn.execute(f"""
        WITH raw AS (
            SELECT
                CAST(ts AS TIMESTAMP) as ts,
                CAST(instance AS VARCHAR) as instance,
                metric,
                CAST(labels->>'value' AS VARCHAR) as kind,
                value
            FROM read_json_auto('{metrics_path_str}')
            WHERE CAST(labels->>'api_call' AS VARCHAR) = 'endpoint-create'
              AND metric IN ('cilium_api_limiter_wait_duration_seconds', 'cilium_api_limiter_rate_limit')
        ),
        binned AS (
            SELECT
                time_bucket(INTERVAL '15 seconds', ts) as bucket_time,
                instance,
                AVG(CASE WHEN metric = 'cilium_api_limiter_wait_duration_seconds' AND kind = 'mean' THEN value END) as wait_mean_s,
                AVG(CASE WHEN metric = 'cilium_api_limiter_rate_limit' AND kind = 'limit' THEN value END) as rate_limit
            FROM raw
            GROUP BY bucket_time, instance
        )
        SELECT
            strftime(bucket_time, '%Y-%m-%dT%H:%M:%SZ') as ts,
            instance,
            wait_mean_s,
            rate_limit
        FROM binned
        WHERE wait_mean_s IS NOT NULL OR rate_limit IS NOT NULL
        ORDER BY ts, instance
    """).fetchall()

    cilium_chart_data = [
        {"ts": row[0], "instance": row[1], "wait_mean_s": row[2], "rate_limit": row[3]}
        for row in cilium_limiter_ts_raw
    ]

    print("Querying cilium endpoint regeneration by phase...")
    cilium_regen_raw = metrics_by_phase(
        conn, metrics_path_str,
        metrics=["cilium_endpoint_regeneration_time_stats_seconds_count",
                 "cilium_endpoint_regeneration_time_stats_seconds_sum"],
        labels={"scope": "scope", "status": "status"},
        group_by=[],
        where="scope = 'total' AND status = 'success'")
    cilium_regen_raw = [r for r in cilium_regen_raw if r[1] > 0]

    cilium_regen = []
    for row in cilium_regen_raw:
        phase_name, count_delta, sum_delta = row
        cilium_regen.append({
            "phase_name": phase_name,
            "count_delta": int(count_delta),
            "avg_ms": (sum_delta / count_delta) * 1000 if count_delta > 0 else 0
        })

    cilium_available = bool(cilium_api_ops or cilium_limiter_summary or cilium_regen)

    # Client-side API rate limiting, system-wide. Every component that talks
    # to the apiserver throttles itself client-side (client-go QPS/burst;
    # cilium-agent's k8s client exposes its own equivalent metric), and an
    # undersized limit shows up as end-to-end latency while the apiserver
    # sits idle. One unified table makes each limit raise provable in the
    # report. Note: the agent-sandbox controller (controller-runtime) does
    # not currently export rest_client rate-limiter metrics.
    print("Querying client-side API throttling by phase...")
    client_ratelimit_raw = metrics_by_phase(
        conn, metrics_path_str,
        metrics=["rest_client_rate_limiter_duration_seconds_count",
                 "rest_client_rate_limiter_duration_seconds_sum"],
        labels={"verb": "verb", "host": "host"},
        group_by=["source", "verb"]) + metrics_by_phase(
        conn, metrics_path_str,
        metrics=["cilium_k8s_client_rate_limiter_duration_seconds_count",
                 "cilium_k8s_client_rate_limiter_duration_seconds_sum"],
        labels={"verb": "method", "path": "path"},
        group_by=["source", "verb"])
    client_ratelimit_raw = [r for r in client_ratelimit_raw if r[3] > 0]
    client_ratelimit_raw.sort(key=lambda r: (r[0], -r[4]))

    client_ratelimit_ops = []
    for row in client_ratelimit_raw:
        phase_name, source, verb, count_delta, wait_total = row
        client_ratelimit_ops.append({
            "phase_name": phase_name,
            "source": source,
            "verb": verb,
            "count_delta": int(count_delta),
            "total_wait_s": wait_total,
            "avg_wait_ms": (wait_total / count_delta) * 1000 if count_delta > 0 else 0
        })

    print("Querying client throttling timeseries...")
    client_ratelimit_ts_raw = metrics_timeseries(
        conn, metrics_path_str,
        metrics=["rest_client_rate_limiter_duration_seconds_count",
                 "rest_client_rate_limiter_duration_seconds_sum"],
        labels={"verb": "verb", "host": "host"},
        group_by=["source"]) + metrics_timeseries(
        conn, metrics_path_str,
        metrics=["cilium_k8s_client_rate_limiter_duration_seconds_count",
                 "cilium_k8s_client_rate_limiter_duration_seconds_sum"],
        labels={"verb": "method", "path": "path"},
        group_by=["source"])
    client_ratelimit_ts_raw.sort(key=lambda r: (r[0], r[1]))

    client_ratelimit_chart_data = [
        {"ts": row[0], "source": row[1],
         "avg_wait_s": row[3] / row[2] if row[2] > 0 else 0.0}
        for row in client_ratelimit_ts_raw
    ]

    # API Priority & Fairness: server-side queueing at the apiserver, the
    # other place where "the cluster feels slow but nothing is busy".
    print("Querying API Priority & Fairness wait by phase...")
    apf_raw = metrics_by_phase(
        conn, metrics_path_str,
        metrics=["apiserver_flowcontrol_request_wait_duration_seconds_count",
                 "apiserver_flowcontrol_request_wait_duration_seconds_sum"],
        labels={"priority_level": "priority_level", "flow_schema": "flow_schema", "execute": "execute"},
        group_by=["priority_level"])
    apf_raw = [r for r in apf_raw if r[2] > 0]
    apf_raw.sort(key=lambda r: (r[0], -r[3]))

    apf_ops = []
    for row in apf_raw:
        phase_name, priority_level, count_delta, wait_total = row
        apf_ops.append({
            "phase_name": phase_name,
            "priority_level": priority_level,
            "count_delta": int(count_delta),
            "total_wait_s": wait_total,
            "avg_wait_ms": (wait_total / count_delta) * 1000 if count_delta > 0 else 0
        })

    # Query active sandboxes and pods capacity timeseries from the watch logs
    capacity_chart_data = []
    capacity_summary = []
    pod_capacity = int(summary.get("cluster", {}).get("podCapacity", 0) or 0)
    cluster_nodes = int(summary.get("cluster", {}).get("nodes", 0) or 0)

    if watch_file.exists():
        watch_path_str = str(watch_file).replace("'", "''")
        print("Querying capacity timeseries from watch stream...")
        capacity_ts_raw = conn.execute(f"""
            WITH raw_events AS (
                SELECT
                    CAST(timestamp AS TIMESTAMP) as ts,
                    resource,
                    -- Key lifecycles by uid: a delete/recreate of the same
                    -- name is two objects, not one long-lived one.
                    CAST(object->'metadata'->>'uid' AS VARCHAR) as uid,
                    type
                FROM read_json_auto('{watch_path_str}')
                WHERE resource IN ('pods', 'sandboxes')
            ),
            lifecycle_ends AS (
                SELECT
                    resource,
                    uid,
                    MIN(CASE WHEN type = 'ADDED' THEN ts ELSE NULL END) as first_seen,
                    MAX(CASE WHEN type = 'DELETED' THEN ts ELSE NULL END) as deleted_at
                FROM raw_events
                GROUP BY resource, uid
            ),
            object_lifecycles AS (
                SELECT
                    resource,
                    COALESCE(first_seen, (SELECT MIN(ts) FROM raw_events)) as created_at,
                    deleted_at
                FROM lifecycle_ends
            ),
            time_series AS (
                -- Sample over the whole run window: undeleted objects would
                -- otherwise end the series at their creation time.
                SELECT ts
                FROM unnest(generate_series(
                    CAST('{start_time_iso}' AS TIMESTAMP),
                    CAST('{end_time_iso}' AS TIMESTAMP),
                    INTERVAL '5 seconds'
                )) as t(ts)
            )
            SELECT 
                strftime(ts, '%Y-%m-%dT%H:%M:%SZ') as time_str,
                epoch(ts - CAST('{start_time_iso}' AS TIMESTAMP)) as offset_sec,
                (
                    SELECT COUNT(*) 
                    FROM object_lifecycles o 
                    WHERE o.resource = 'sandboxes' 
                      AND o.created_at <= ts 
                      AND (o.deleted_at IS NULL OR o.deleted_at >= ts)
                ) as active_sandboxes,
                (
                    SELECT COUNT(*) 
                    FROM object_lifecycles o 
                    WHERE o.resource = 'pods' 
                      AND o.created_at <= ts 
                      AND (o.deleted_at IS NULL OR o.deleted_at >= ts)
                ) as active_pods
            FROM time_series
            ORDER BY ts
        """).fetchall()

        for row in capacity_ts_raw:
            capacity_chart_data.append({
                "ts": row[0],
                "offset_sec": row[1],
                "active_sandboxes": int(row[2]),
                "active_pods": int(row[3])
            })

        print("Querying peak workload density by phase from watch stream...")
        capacity_summary_raw = conn.execute(f"""
            WITH raw_events AS (
                SELECT
                    CAST(timestamp AS TIMESTAMP) as ts,
                    resource,
                    -- Key lifecycles by uid: a delete/recreate of the same
                    -- name is two objects, not one long-lived one.
                    CAST(object->'metadata'->>'uid' AS VARCHAR) as uid,
                    type
                FROM read_json_auto('{watch_path_str}')
                WHERE resource IN ('pods', 'sandboxes')
            ),
            lifecycle_ends AS (
                SELECT
                    resource,
                    uid,
                    MIN(CASE WHEN type = 'ADDED' THEN ts ELSE NULL END) as first_seen,
                    MAX(CASE WHEN type = 'DELETED' THEN ts ELSE NULL END) as deleted_at
                FROM raw_events
                GROUP BY resource, uid
            ),
            object_lifecycles AS (
                SELECT
                    resource,
                    COALESCE(first_seen, (SELECT MIN(ts) FROM raw_events)) as created_at,
                    deleted_at
                FROM lifecycle_ends
            ),
            time_series AS (
                -- Sample over the whole run window: undeleted objects would
                -- otherwise end the series at their creation time.
                SELECT ts
                FROM unnest(generate_series(
                    CAST('{start_time_iso}' AS TIMESTAMP),
                    CAST('{end_time_iso}' AS TIMESTAMP),
                    INTERVAL '5 seconds'
                )) as t(ts)
            ),
            ts_counts AS (
                SELECT 
                    ts,
                    (
                        SELECT COUNT(*) 
                        FROM object_lifecycles o 
                        WHERE o.resource = 'sandboxes' 
                          AND o.created_at <= ts 
                          AND (o.deleted_at IS NULL OR o.deleted_at >= ts)
                    ) as active_sandboxes,
                    (
                        SELECT COUNT(*) 
                        FROM object_lifecycles o 
                        WHERE o.resource = 'pods' 
                          AND o.created_at <= ts 
                          AND (o.deleted_at IS NULL OR o.deleted_at >= ts)
                    ) as active_pods
                FROM time_series
            )
            SELECT 
                p.name as phase_name,
                MAX(t.active_sandboxes) as peak_sandboxes,
                MAX(t.active_pods) as peak_pods
            FROM ts_counts t
            JOIN phases p ON t.ts >= p.start_time AND t.ts < p.end_time
            GROUP BY p.name
            ORDER BY MIN(p.start_time)
        """).fetchall()

        for row in capacity_summary_raw:
            capacity_summary.append({
                "phase_name": row[0],
                "peak_sandboxes": int(row[1]) if row[1] is not None else 0,
                "peak_pods": int(row[2]) if row[2] is not None else 0,
                "limit": pod_capacity
            })

    # 4. Analyzer rules to identify findings
    findings = []

    # CRI check
    cri_run_pod_latency_max = 0.0
    for op in cri_ops:
        if op['operation_type'] == 'run_podsandbox' and op['phase_name'].startswith('throughput'):
            if op['avg_latency_ms'] > cri_run_pod_latency_max:
                cri_run_pod_latency_max = op['avg_latency_ms']

    if cri_run_pod_latency_max > 5000:
        findings.append({
            "severity": "critical",
            "title": f"CRI Pod Sandbox Creation Bottleneck ({cri_run_pod_latency_max/1000:.1f}s)",
            "desc": f"Kubelet run_podsandbox average latency spiked to {cri_run_pod_latency_max/1000:.1f} seconds during throughput phases. This indicates a container runtime (gVisor/containerd) or network/CNI (Cilium) setup bottleneck under load.",
            "link": "cri.html"
        })

    # Controller check: ratio of reconciles per sandbox created
    phase_created_map = {p['name']: p.get('created', 0) for p in summary.get('phases', [])}
    
    max_reconciles_per_created = 0.0
    max_reconciles_per_created_phase = ""
    max_reconciles = 0
    max_created = 0
    
    for row in controller_ops:
        if row['controller'] == 'sandbox' and row['phase_name'].startswith('throughput'):
            phase_name = row['phase_name']
            created = phase_created_map.get(phase_name, 0)
            if created > 0:
                ratio = row['total_reconciles'] / created
                if ratio > max_reconciles_per_created:
                    max_reconciles_per_created = ratio
                    max_reconciles_per_created_phase = phase_name
                    max_reconciles = row['total_reconciles']
                    max_created = created

    if max_reconciles_per_created > 20.0:
        findings.append({
            "severity": "warning",
            "title": f"High Reconciler Churn ({max_reconciles_per_created:.1f} reconciles/object)",
            "desc": f"The Sandbox controller reconciliation count per sandbox object created reached {max_reconciles_per_created:.1f} in phase {max_reconciles_per_created_phase} ({max_reconciles:,} reconciles for {max_created:,} objects). This indicates high reconciliation redundancy, where a single sandbox object launch triggers excessive watch event updates in a short window.",
            "link": "agent-sandbox-controller.html"
        })

    # etcd check
    etcd_update_latency_max = 0.0
    for row in etcd_ops:
        if row['operation'] == 'update' and row['phase_name'].startswith('throughput'):
            if row['avg_latency_ms'] > etcd_update_latency_max:
                etcd_update_latency_max = row['avg_latency_ms']

    if etcd_update_latency_max > 5.0:
        findings.append({
            "severity": "warning",
            "title": f"Elevated etcd Update Latency ({etcd_update_latency_max:.2f}ms)",
            "desc": f"etcd update operation average latency reached {etcd_update_latency_max:.2f}ms under load. While standard, high write latencies from etcd indicate write disk throughput contention.",
            "link": "etcd.html"
        })

    # Cilium endpoint-create rate limiting check: launch latency spent
    # queueing in the agent's api-rate-limit, not doing CNI work.
    # Two distinct failure modes, tracked independently so the worst phase
    # for each is evaluated: time queued in the api-rate-limit (raise the
    # limit) vs time spent actually processing the create (the limiter is
    # fine; look at what endpoint creation is doing).
    cilium_wait_worst = None
    cilium_processing_worst = None
    for row in cilium_limiter_summary:
        if row['phase_name'].startswith('throughput'):
            if cilium_wait_worst is None or row['wait_mean_s'] > cilium_wait_worst['wait_mean_s']:
                cilium_wait_worst = row
            if cilium_processing_worst is None or row['processing_mean_s'] > cilium_processing_worst['processing_mean_s']:
                cilium_processing_worst = row

    if (cilium_wait_worst and cilium_wait_worst['wait_mean_s'] > 0.5
            and cilium_wait_worst['wait_mean_s'] >= cilium_wait_worst['processing_mean_s']):
        w = cilium_wait_worst
        findings.append({
            "severity": "critical" if w['wait_mean_s'] > 5.0 else "warning",
            "title": f"Cilium endpoint-create API Rate Limited ({w['wait_mean_s']:.1f}s mean wait)",
            "desc": f"During phase {w['phase_name']}, CNI endpoint-create requests waited a mean of {w['wait_mean_s']:.1f}s (max {w['wait_max_s']:.0f}s) in cilium-agent's API rate limiter, while actual processing took {w['processing_mean_s']:.2f}s. The effective limit averaged {w['rate_limit']:.1f} creates/s per node, which caps pod sandbox creation throughput. Consider raising the endpoint-create limits in Cilium's api-rate-limit configuration.",
            "link": "cilium.html"
        })
    elif cilium_processing_worst and cilium_processing_worst['processing_mean_s'] > 1.0:
        w = cilium_processing_worst
        findings.append({
            "severity": "critical" if w['processing_mean_s'] > 5.0 else "warning",
            "title": f"Cilium endpoint-create Slow ({w['processing_mean_s']:.1f}s mean processing)",
            "desc": f"During phase {w['phase_name']}, cilium-agent spent a mean of {w['processing_mean_s']:.1f}s processing each endpoint create (limiter wait was only {w['wait_mean_s']:.1f}s, so api-rate-limit is not the cap). The time is going into the endpoint-create pipeline itself — check client-side API throttling on the Rate Limiting page, endpoint regeneration on the Cilium page, and apiserver latency.",
            "link": "cilium.html"
        })

    # Client-side API throttling check: a component sitting in its own
    # client-go rate limiter while the apiserver is idle.
    client_throttle_worst = None
    for row in client_ratelimit_ops:
        if row['phase_name'].startswith('throughput') and row['count_delta'] >= 20:
            if client_throttle_worst is None or row['avg_wait_ms'] > client_throttle_worst['avg_wait_ms']:
                client_throttle_worst = row

    if client_throttle_worst and client_throttle_worst['avg_wait_ms'] > 100:
        w = client_throttle_worst
        findings.append({
            "severity": "critical" if w['avg_wait_ms'] > 1000 else "warning",
            "title": f"Client-side API Throttling in {w['source']} ({w['avg_wait_ms']/1000:.2f}s avg wait)",
            "desc": f"During phase {w['phase_name']}, {w['count_delta']:,} {w['verb']} requests from {w['source']} waited an average of {w['avg_wait_ms']/1000:.2f}s ({w['total_wait_s']:.0f}s in total) in the component's own client-side rate limiter before being sent to the apiserver. Consider raising that component's client QPS/burst configuration.",
            "link": "ratelimits.html"
        })

    # Capacity saturation finding check
    max_active_pods = 0
    for pt in capacity_chart_data:
        if pt['active_pods'] > max_active_pods:
            max_active_pods = pt['active_pods']

    if pod_capacity > 0:
        if max_active_pods >= pod_capacity:
            findings.append({
                "severity": "critical",
                "title": f"Cluster Pod Capacity Exhausted ({max_active_pods} / {pod_capacity})",
                "desc": f"The number of active workload pods peaked at {max_active_pods}, exceeding the cluster's physical capacity limit of {pod_capacity} pods across {cluster_nodes} worker nodes. This causes scheduling delays (pods stuck in Pending) and saturates Kubelet / containerd resource limits.",
                "link": "capacity.html"
            })
        elif max_active_pods >= 0.9 * pod_capacity:
            findings.append({
                "severity": "warning",
                "title": f"Cluster Pod Capacity Saturated ({max_active_pods} / {pod_capacity})",
                "desc": f"The number of active workload pods peaked at {max_active_pods}, reaching {max_active_pods / pod_capacity * 100:.1f}% of the cluster's physical capacity limit of {pod_capacity} pods. High density triggers CNI IPAM queuing and Kubelet slow downs.",
                "link": "capacity.html"
            })

    # Query sandbox launch latency percentiles by phase
    print("Querying sandbox percentiles...")
    sandbox_percentiles_raw = []
    if sandboxes_file.exists():
        sandboxes_path_str = str(sandboxes_file).replace("'", "''")
        sandbox_percentiles_raw = conn.execute(f"""
            SELECT 
                phase,
                count(*) as count,
                quantile_cont(createAckMs, 0.5) as createAck_p50,
                quantile_cont(createAckMs, 0.9) as createAck_p90,
                quantile_cont(podCreatedMs, 0.5) as podCreated_p50,
                quantile_cont(podCreatedMs, 0.9) as podCreated_p90,
                quantile_cont(podScheduledMs, 0.5) as podScheduled_p50,
                quantile_cont(podScheduledMs, 0.9) as podScheduled_p90,
                quantile_cont(podRunningMs, 0.5) as podRunning_p50,
                quantile_cont(podRunningMs, 0.9) as podRunning_p90,
                quantile_cont(sandboxReadyMs, 0.5) as sandboxReady_p50,
                quantile_cont(sandboxReadyMs, 0.9) as sandboxReady_p90,
                quantile_cont(sandboxReadyMs, 0.99) as sandboxReady_p99
            FROM read_json_auto('{sandboxes_path_str}')
            GROUP BY phase
            ORDER BY phase
        """).fetchall()

    sandbox_percentiles = []
    for row in sandbox_percentiles_raw:
        sandbox_percentiles.append({
            "phase": row[0],
            "count": int(row[1]),
            "createAck_p50": row[2],
            "createAck_p90": row[3],
            "podCreated_p50": row[4],
            "podCreated_p90": row[5],
            "podScheduled_p50": row[6],
            "podScheduled_p90": row[7],
            "podRunning_p50": row[8],
            "podRunning_p90": row[9],
            "sandboxReady_p50": row[10],
            "sandboxReady_p90": row[11],
            "sandboxReady_p99": row[12]
        })

    # Discover CPU profiles captured by the stress tool (standard pprof
    # format: gzip-compressed profile.proto). They are copied next to the
    # report pages and parsed/rendered client-side by pprof.html.
    pprof_files = sorted(
        list(input_dir.glob("pprof-*.pprof"))
        + list(input_dir.glob("pprof-*.pb"))
        + list(input_dir.glob("pprof-*.pb.gz")))
    phase_order = {p["name"]: i for i, p in enumerate(summary.get("phases", []))}
    pprof_profiles = []
    for f in pprof_files:
        label = f.name[len("pprof-"):]
        for suffix in (".pprof", ".pb.gz", ".pb"):
            if label.endswith(suffix):
                label = label[:-len(suffix)]
                break
        # Order profiles by when their phase ran, not alphabetically.
        order = next((i for name, i in phase_order.items() if label.endswith(name)), len(phase_order))
        pprof_profiles.append({"file": f.name, "label": label, "order": order})
    pprof_profiles.sort(key=lambda p: (p["order"], p["label"]))

    # 5. Render Templates using Jinja2
    output_dir.mkdir(parents=True, exist_ok=True)
    template_dir = Path(__file__).parent / "templates"
    env = Environment(loader=FileSystemLoader(template_dir), autoescape=select_autoescape(['html']))

    # Copy shared static assets (CSS / JS) referenced by the pages
    static_dir = Path(__file__).parent / "static"
    for static_file in sorted(static_dir.iterdir()):
        if static_file.is_file():
            shutil.copy(static_file, output_dir / static_file.name)
            print(f"Copied static asset: {output_dir / static_file.name}")

    # Copy the profiles so pprof.html can fetch them relative to itself
    for f in pprof_files:
        shutil.copy(f, output_dir / f.name)
        print(f"Copied CPU profile: {output_dir / f.name}")

    def render_page(template_name, output_filename, context):
        template = env.get_template(template_name)
        rendered = template.render(context)
        output_file = output_dir / output_filename
        with open(output_file, 'w') as f:
            f.write(rendered)
        print(f"Generated report page: {output_file}")

    # Index context
    index_ctx = {
        "active_page": "index",
        "summary": summary,
        "run_duration": run_duration,
        "total_created": total_created,
        "probe_latency_ms": probe_latency_ms,
        "findings": findings,
        "sandbox_percentiles": sandbox_percentiles
    }
    render_page("index.html", "index.html", index_ctx)

    # CRI context
    cri_ctx = {
        "active_page": "cri",
        "summary": summary,
        "cri_ops": cri_ops,
        "chart_data": cri_chart_data,
        "phases": js_phases
    }
    render_page("cri.html", "cri.html", cri_ctx)

    # Controller context
    controller_ctx = {
        "active_page": "controller",
        "summary": summary,
        "controller_ops": controller_ops,
        "chart_data": controller_chart_data,
        "phases": js_phases
    }
    render_page("agent-sandbox-controller.html", "agent-sandbox-controller.html", controller_ctx)

    # Apiserver context
    apiserver_ctx = {
        "active_page": "apiserver",
        "summary": summary,
        "apiserver_ops": apiserver_ops,
        "chart_data": apiserver_chart_data,
        "phases": js_phases
    }
    render_page("apiserver.html", "apiserver.html", apiserver_ctx)

    # etcd context
    etcd_ctx = {
        "active_page": "etcd",
        "summary": summary,
        "etcd_ops": etcd_ops,
        "chart_data": etcd_chart_data,
        "phases": js_phases
    }
    render_page("etcd.html", "etcd.html", etcd_ctx)

    # Cilium context
    cilium_ctx = {
        "active_page": "cilium",
        "summary": summary,
        "cilium_available": cilium_available,
        "cilium_api_ops": cilium_api_ops,
        "cilium_limiter_summary": cilium_limiter_summary,
        "cilium_regen": cilium_regen,
        "chart_data": cilium_chart_data,
        "phases": js_phases
    }
    render_page("cilium.html", "cilium.html", cilium_ctx)

    # Rate limiting context
    ratelimits_ctx = {
        "active_page": "ratelimits",
        "summary": summary,
        "client_ratelimit_ops": client_ratelimit_ops,
        "apf_ops": apf_ops,
        "chart_data": client_ratelimit_chart_data,
        "phases": js_phases
    }
    render_page("ratelimits.html", "ratelimits.html", ratelimits_ctx)

    # Capacity context
    capacity_ctx = {
        "active_page": "capacity",
        "summary": summary,
        "pod_capacity": pod_capacity,
        "chart_data": capacity_chart_data,
        "capacity_summary": capacity_summary,
        "phases": js_phases
    }
    render_page("capacity.html", "capacity.html", capacity_ctx)

    # CPU profiles context
    pprof_ctx = {
        "active_page": "pprof",
        "summary": summary,
        "pprof_profiles": pprof_profiles
    }
    render_page("pprof.html", "pprof.html", pprof_ctx)

    print("All report pages generated successfully!")

if __name__ == "__main__":
    main()
