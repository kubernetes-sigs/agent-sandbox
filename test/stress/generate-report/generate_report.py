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
    cri_ops_raw = conn.execute(f"""
        WITH raw AS (
            SELECT
                CAST(ts AS TIMESTAMP) as ts,
                CAST(instance AS VARCHAR) as instance,
                CAST(labels->>'operation_type' AS VARCHAR) as operation_type,
                metric,
                value
            FROM read_json_auto('{metrics_path_str}')
            WHERE metric IN ('kubelet_runtime_operations_duration_seconds_count', 'kubelet_runtime_operations_duration_seconds_sum')
        ),
        diffs AS (
            SELECT
                ts,
                operation_type,
                metric,
                value,
                value - lag(value) OVER (PARTITION BY instance, operation_type, metric ORDER BY ts) as delta
            FROM raw
        ),
        diffs_with_phase AS (
            SELECT 
                p.name as phase_name,
                d.operation_type,
                d.metric,
                d.delta
            FROM diffs d
            JOIN phases p ON d.ts >= p.start_time AND d.ts < p.end_time
            WHERE d.delta >= 0
        )
        SELECT 
            phase_name,
            operation_type,
            SUM(CASE WHEN metric = 'kubelet_runtime_operations_duration_seconds_count' THEN delta ELSE 0 END) as count_delta,
            SUM(CASE WHEN metric = 'kubelet_runtime_operations_duration_seconds_sum' THEN delta ELSE 0 END) as sum_delta
        FROM diffs_with_phase
        GROUP BY phase_name, operation_type
        HAVING count_delta > 0
        ORDER BY phase_name, count_delta DESC
    """).fetchall()

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
    cri_ts_raw = conn.execute(f"""
        WITH raw AS (
            SELECT 
                CAST(ts AS TIMESTAMP) as ts,
                CAST(instance AS VARCHAR) as instance,
                metric,
                value
            FROM read_json_auto('{metrics_path_str}')
            WHERE metric IN ('kubelet_runtime_operations_duration_seconds_count', 'kubelet_runtime_operations_duration_seconds_sum')
              AND CAST(labels->>'operation_type' AS VARCHAR) = 'run_podsandbox'
        ),
        diffs AS (
            SELECT
                ts,
                instance,
                metric,
                value - lag(value) OVER (PARTITION BY instance, metric ORDER BY ts) as delta
            FROM raw
        ),
        binned AS (
            SELECT
                time_bucket(INTERVAL '15 seconds', ts) as bucket_time,
                instance,
                SUM(CASE WHEN metric = 'kubelet_runtime_operations_duration_seconds_count' THEN delta ELSE 0 END) as count_delta,
                SUM(CASE WHEN metric = 'kubelet_runtime_operations_duration_seconds_sum' THEN delta ELSE 0 END) as sum_delta
            FROM diffs
            WHERE delta >= 0
            GROUP BY bucket_time, instance
        )
        SELECT 
            strftime(bucket_time, '%Y-%m-%dT%H:%M:%SZ') as ts,
            instance,
            count_delta,
            CASE WHEN count_delta > 0 THEN sum_delta / count_delta ELSE 0.0 END as avg_latency_s
        FROM binned
        ORDER BY ts, instance
    """).fetchall()

    cri_chart_data = [
        {"ts": row[0], "instance": row[1], "count": int(row[2]), "avg_latency_s": row[3]}
        for row in cri_ts_raw
    ]

    print("Querying controller performance by phase...")
    controller_ops_raw = conn.execute(f"""
        WITH raw AS (
            SELECT
                CAST(ts AS TIMESTAMP) as ts,
                CAST(instance AS VARCHAR) as instance,
                CAST(labels->>'controller' AS VARCHAR) as controller,
                metric,
                COALESCE(CAST(labels->>'result' AS VARCHAR), '') as result,
                value
            FROM read_json_auto('{metrics_path_str}')
            WHERE metric IN ('controller_runtime_reconcile_total', 'controller_runtime_reconcile_errors_total', 'controller_runtime_reconcile_time_seconds_sum', 'controller_runtime_reconcile_time_seconds_count')
        ),
        diffs AS (
            SELECT
                ts,
                controller,
                metric,
                result,
                value - lag(value) OVER (PARTITION BY instance, controller, metric, result ORDER BY ts) as delta
            FROM raw
        ),
        diffs_with_phase AS (
            SELECT 
                p.name as phase_name,
                d.controller,
                d.metric,
                d.delta
            FROM diffs d
            JOIN phases p ON d.ts >= p.start_time AND d.ts < p.end_time
            WHERE d.delta >= 0
        )
        SELECT 
            phase_name,
            controller,
            SUM(CASE WHEN metric = 'controller_runtime_reconcile_total' THEN delta ELSE 0 END) as total_reconciles,
            SUM(CASE WHEN metric = 'controller_runtime_reconcile_errors_total' THEN delta ELSE 0 END) as total_errors,
            SUM(CASE WHEN metric = 'controller_runtime_reconcile_time_seconds_sum' THEN delta ELSE 0 END) as sum_time,
            SUM(CASE WHEN metric = 'controller_runtime_reconcile_time_seconds_count' THEN delta ELSE 0 END) as count_time
        FROM diffs_with_phase
        GROUP BY phase_name, controller
        ORDER BY phase_name, controller
    """).fetchall()

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
    controller_ts_raw = conn.execute(f"""
        WITH raw AS (
            SELECT
                CAST(ts AS TIMESTAMP) as ts,
                CAST(instance AS VARCHAR) as instance,
                CAST(labels->>'controller' AS VARCHAR) as controller,
                CAST(labels->>'result' AS VARCHAR) as result,
                value
            FROM read_json_auto('{metrics_path_str}')
            WHERE metric = 'controller_runtime_reconcile_total'
        ),
        diffs AS (
            SELECT
                ts,
                controller,
                result,
                value - lag(value) OVER (PARTITION BY instance, controller, result ORDER BY ts) as delta
            FROM raw
        ),
        binned AS (
            SELECT
                time_bucket(INTERVAL '15 seconds', ts) as bucket_time,
                controller,
                SUM(delta) as total_reconciles
            FROM diffs
            WHERE delta >= 0
            GROUP BY bucket_time, controller
        )
        SELECT 
            strftime(bucket_time, '%Y-%m-%dT%H:%M:%SZ') as ts,
            controller,
            total_reconciles / 15.0 as reconcile_rate
        FROM binned
        ORDER BY ts, controller
    """).fetchall()

    controller_chart_data = [
        {"ts": row[0], "controller": row[1], "reconcile_rate": row[2]}
        for row in controller_ts_raw
    ]

    print("Querying apiserver operations by phase...")
    apiserver_ops_raw = conn.execute(f"""
        WITH raw AS (
            SELECT 
                CAST(ts AS TIMESTAMP) as ts,
                CAST(labels->>'resource' AS VARCHAR) as resource,
                CAST(labels->>'subresource' AS VARCHAR) as subresource,
                CAST(labels->>'verb' AS VARCHAR) as verb,
                CAST(labels->>'group' AS VARCHAR) as group_label,
                CAST(labels->>'version' AS VARCHAR) as version,
                CAST(labels->>'scope' AS VARCHAR) as scope,
                CAST(labels->>'dry_run' AS VARCHAR) as dry_run,
                CAST(instance AS VARCHAR) as instance,
                metric,
                value
            FROM read_json_auto('{metrics_path_str}')
            WHERE metric IN ('apiserver_request_duration_seconds_count', 'apiserver_request_duration_seconds_sum')
        ),
        diffs AS (
            SELECT
                ts,
                resource,
                verb,
                metric,
                value - lag(value) OVER (PARTITION BY instance, group_label, version, resource, subresource, scope, verb, dry_run, metric ORDER BY ts) as delta
            FROM raw
        ),
        diffs_with_phase AS (
            SELECT 
                p.name as phase_name,
                d.resource,
                d.verb,
                d.metric,
                d.delta
            FROM diffs d
            JOIN phases p ON d.ts >= p.start_time AND d.ts < p.end_time
            WHERE d.delta >= 0
        )
        SELECT 
            phase_name,
            resource,
            verb,
            SUM(CASE WHEN metric = 'apiserver_request_duration_seconds_count' THEN delta ELSE 0 END) as count_delta,
            SUM(CASE WHEN metric = 'apiserver_request_duration_seconds_sum' THEN delta ELSE 0 END) as sum_delta
        FROM diffs_with_phase
        GROUP BY phase_name, resource, verb
        HAVING count_delta > 0
        ORDER BY phase_name, count_delta DESC
    """).fetchall()

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
    apiserver_ts_raw = conn.execute(f"""
        WITH raw AS (
            SELECT 
                CAST(ts AS TIMESTAMP) as ts,
                CAST(labels->>'resource' AS VARCHAR) as resource,
                CAST(labels->>'subresource' AS VARCHAR) as subresource,
                CAST(labels->>'verb' AS VARCHAR) as verb,
                CAST(labels->>'group' AS VARCHAR) as group_label,
                CAST(labels->>'version' AS VARCHAR) as version,
                CAST(labels->>'scope' AS VARCHAR) as scope,
                CAST(labels->>'dry_run' AS VARCHAR) as dry_run,
                CAST(instance AS VARCHAR) as instance,
                value
            FROM read_json_auto('{metrics_path_str}')
            WHERE metric = 'apiserver_request_duration_seconds_count'
        ),
        diffs AS (
            SELECT
                ts,
                resource,
                verb,
                value - lag(value) OVER (PARTITION BY instance, group_label, version, resource, subresource, scope, verb, dry_run ORDER BY ts) as delta
            FROM raw
        ),
        binned AS (
            SELECT
                time_bucket(INTERVAL '15 seconds', ts) as bucket_time,
                resource,
                verb,
                SUM(delta) as total_requests
            FROM diffs
            WHERE delta >= 0
            GROUP BY bucket_time, resource, verb
        )
        SELECT 
            strftime(bucket_time, '%Y-%m-%dT%H:%M:%SZ') as ts,
            resource,
            verb,
            total_requests / 15.0 as request_rate
        FROM binned
        ORDER BY ts, resource, verb
    """).fetchall()

    apiserver_chart_data = [
        {"ts": row[0], "resource": row[1], "verb": row[2], "request_rate": row[3]}
        for row in apiserver_ts_raw
    ]

    print("Querying etcd operations by phase...")
    etcd_ops_raw = conn.execute(f"""
        WITH raw AS (
            SELECT 
                CAST(ts AS TIMESTAMP) as ts,
                CAST(labels->>'resource' AS VARCHAR) as resource,
                CAST(labels->>'operation' AS VARCHAR) as operation,
                CAST(labels->>'group' AS VARCHAR) as group_label,
                CAST(instance AS VARCHAR) as instance,
                metric,
                value
            FROM read_json_auto('{metrics_path_str}')
            WHERE metric IN ('etcd_request_duration_seconds_count', 'etcd_request_duration_seconds_sum')
        ),
        diffs AS (
            SELECT
                ts,
                resource,
                operation,
                metric,
                value - lag(value) OVER (PARTITION BY instance, group_label, resource, operation, metric ORDER BY ts) as delta
            FROM raw
        ),
        diffs_with_phase AS (
            SELECT 
                p.name as phase_name,
                d.resource,
                d.operation,
                d.metric,
                d.delta
            FROM diffs d
            JOIN phases p ON d.ts >= p.start_time AND d.ts < p.end_time
            WHERE d.delta >= 0
        )
        SELECT 
            phase_name,
            resource,
            operation,
            SUM(CASE WHEN metric = 'etcd_request_duration_seconds_count' THEN delta ELSE 0 END) as count_delta,
            SUM(CASE WHEN metric = 'etcd_request_duration_seconds_sum' THEN delta ELSE 0 END) as sum_delta
        FROM diffs_with_phase
        GROUP BY phase_name, resource, operation
        HAVING count_delta > 0
        ORDER BY phase_name, count_delta DESC
    """).fetchall()

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
    etcd_ts_raw = conn.execute(f"""
        WITH raw AS (
            SELECT 
                CAST(ts AS TIMESTAMP) as ts,
                CAST(labels->>'resource' AS VARCHAR) as resource,
                CAST(labels->>'operation' AS VARCHAR) as operation,
                CAST(labels->>'group' AS VARCHAR) as group_label,
                CAST(instance AS VARCHAR) as instance,
                value
            FROM read_json_auto('{metrics_path_str}')
            WHERE metric = 'etcd_request_duration_seconds_count'
        ),
        diffs AS (
            SELECT
                ts,
                resource,
                operation,
                value - lag(value) OVER (PARTITION BY instance, group_label, resource, operation ORDER BY ts) as delta
            FROM raw
        ),
        binned AS (
            SELECT
                time_bucket(INTERVAL '15 seconds', ts) as bucket_time,
                resource,
                operation,
                SUM(delta) as total_requests
            FROM diffs
            WHERE delta >= 0
            GROUP BY bucket_time, resource, operation
        )
        SELECT 
            strftime(bucket_time, '%Y-%m-%dT%H:%M:%SZ') as ts,
            resource,
            operation,
            total_requests / 15.0 as request_rate
        FROM binned
        ORDER BY ts, resource, operation
    """).fetchall()

    etcd_chart_data = [
        {"ts": row[0], "resource": row[1], "operation": row[2], "request_rate": row[3]}
        for row in etcd_ts_raw
    ]

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

    print("All report pages generated successfully!")

if __name__ == "__main__":
    main()
