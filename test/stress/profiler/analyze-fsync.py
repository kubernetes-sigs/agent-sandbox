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

"""Analyze node-profiler fsync data from a stress-test artifacts directory.

Answers: is fsync (or any other blocking syscall) a meaningful cost on the
nodes during the stress test phases?

Inputs (in the artifacts dir): summary.json, profiler-fsync-<node>.log
(bpftrace output from test/stress/profiler/profiler.yaml).

The bpftrace maps are cumulative since tracer start; this script computes
per-30s-window deltas, attributes windows to test phases by midpoint, and
reports per-phase totals. Requires: pip install duckdb.

Usage:
  analyze-fsync.py <artifacts-dir> [--db out.duckdb]

With --db, the parsed tables (fsync_snapshots, phases) are persisted for
ad-hoc DuckDB queries afterwards.
"""

import argparse
import glob
import json
import os
import re
import sys
from datetime import datetime, timedelta, timezone

import duckdb

# x86_64 syscall numbers, for translating @slow_syscall_* keys.
SYSCALL_NAMES = {
    0: "read", 1: "write", 2: "open", 3: "close", 7: "poll", 9: "mmap",
    16: "ioctl", 17: "pread64", 18: "pwrite64", 20: "writev", 23: "select",
    24: "sched_yield", 34: "pause", 35: "nanosleep", 41: "socket",
    42: "connect", 43: "accept", 44: "sendto", 45: "recvfrom", 46: "sendmsg",
    47: "recvmsg", 56: "clone", 57: "fork", 59: "execve", 61: "wait4",
    72: "fcntl", 73: "flock", 74: "fsync", 75: "fdatasync", 76: "truncate",
    77: "ftruncate", 82: "rename", 83: "mkdir", 84: "rmdir", 86: "link",
    87: "unlink", 90: "chmod", 92: "chown", 128: "rt_sigtimedwait",
    165: "mount", 166: "umount2", 202: "futex", 217: "getdents64",
    230: "clock_nanosleep", 232: "epoll_wait", 233: "epoll_ctl",
    247: "waitid", 257: "openat", 258: "mkdirat", 262: "newfstatat",
    263: "unlinkat", 266: "symlinkat", 267: "readlinkat", 270: "pselect6",
    271: "ppoll", 277: "sync_file_range", 280: "utimensat",
    281: "epoll_pwait", 285: "fallocate", 288: "accept4", 306: "syncfs",
    316: "renameat2", 426: "io_uring_enter", 436: "close_range",
    439: "faccessat2",
}

# Syscalls that block because the process is *waiting for something to
# happen* (event loops, child processes, timers, socket reads). These are
# expected to dominate blocked time and say nothing about work cost, so the
# "work" ranking excludes them.
WAIT_SYSCALLS = {
    "read", "poll", "select", "pselect6", "ppoll", "epoll_wait",
    "epoll_pwait", "futex", "nanosleep", "clock_nanosleep", "wait4",
    "waitid", "rt_sigtimedwait", "accept", "accept4", "recvfrom", "recvmsg",
    "pause", "sched_yield", "io_uring_enter",
}

SNAPSHOT_RE = re.compile(r"^=== snapshot (\d\d:\d\d:\d\d) ===")
# @calls[containerd, tracepoint:syscalls:sys_enter_fdatasync]: 966
# @blocked_us_total[containerd]: 1257073
# @slow_syscall_us_total[containerd-shim, 202]: 14998793135
MAP_RE = re.compile(r"^@(\w+)\[([^\]]*)\]:\s+(\d+)\s*$")


def parse_fsync_log(path, node, run_date):
    """Yields (node, ts, map_name, comm, key, value) rows; values cumulative."""
    ts = None
    # Snapshot headers carry only HH:MM:SS. Snapshots are monotonic within a
    # file, so a time-of-day that goes backwards means the clock crossed
    # midnight; advance the date so a run spanning midnight keeps ascending
    # timestamps instead of jumping back 24h.
    day = run_date
    prev_tod = None
    for line in open(path, errors="replace"):
        m = SNAPSHOT_RE.match(line)
        if m:
            tod = datetime.strptime(m.group(1), "%H:%M:%S").time()
            if prev_tod is not None and tod < prev_tod:
                day = day + timedelta(days=1)
            prev_tod = tod
            ts = datetime.combine(day, tod, tzinfo=timezone.utc)
            continue
        if ts is None:
            continue
        m = MAP_RE.match(line)
        if not m:
            continue  # histogram lines etc.
        map_name, subject, value = m.group(1), m.group(2), int(m.group(3))
        parts = [p.strip() for p in subject.split(",")]
        comm = parts[0]
        key = parts[1] if len(parts) > 1 else ""
        if map_name == "calls":
            key = key.rsplit("sys_enter_", 1)[-1]  # probe name -> syscall
        elif map_name.startswith("slow_syscall"):
            key = SYSCALL_NAMES.get(int(key), f"syscall_{key}")
        yield (node, ts, map_name, comm, key, value)



def _cell(v):
    return f"{v:.1f}" if isinstance(v, float) else str(v)


def dump(con, sql):
    res = con.execute(sql)
    cols = [d[0] for d in res.description]
    rows = res.fetchall()
    widths = [max(len(c), *[len(_cell(r[i])) for r in rows]) if rows else len(c) for i, c in enumerate(cols)]
    print("  ".join(c.rjust(w) for c, w in zip(cols, widths)))
    for r in rows:
        print("  ".join(_cell(v).rjust(w) for v, w in zip(r, widths)))


def load(con, artifacts_dir):
    summary = json.load(open(os.path.join(artifacts_dir, "summary.json")))
    start = datetime.fromisoformat(summary["startTime"].replace("Z", "+00:00"))

    # Phases run sequentially from startTime. Newer summaries carry explicit
    # startOffsetSeconds (needed for max-in-flight sweeps with dynamic phase
    # names); older ones imply order fill -> probe -> throughput.
    phases = []
    entries = summary["phases"]
    # Current schema: a list of {"name": ...}; older runs used a name-keyed dict.
    if isinstance(entries, list):
        entries = {p["name"]: p for p in entries}
    if all("startOffsetSeconds" in p for p in entries.values()):
        for name, p in sorted(entries.items(), key=lambda kv: kv[1]["startOffsetSeconds"]):
            st = start + timedelta(seconds=p["startOffsetSeconds"])
            phases.append((name, st, st + timedelta(seconds=p["durationSeconds"]), p.get("created", 0)))
    else:
        t = start
        for name in ("fill", "probe", "throughput"):
            p = entries.get(name)
            if not p:
                continue
            end = t + timedelta(seconds=p["durationSeconds"])
            phases.append((name, t, end, p.get("created", 0)))
            t = end

    rows = []
    for path in sorted(glob.glob(os.path.join(artifacts_dir, "profiler-fsync-*.log"))):
        node = os.path.basename(path)[len("profiler-fsync-"):-len(".log")]
        rows.extend(parse_fsync_log(path, node, start.date()))
    if not rows:
        sys.exit(f"no profiler-fsync-*.log files found in {artifacts_dir}")

    con.execute("""CREATE OR REPLACE TABLE fsync_snapshots
        (node VARCHAR, ts TIMESTAMPTZ, map VARCHAR, comm VARCHAR,
         key VARCHAR, value BIGINT)""")
    con.executemany("INSERT INTO fsync_snapshots VALUES (?,?,?,?,?,?)", rows)
    con.execute("""CREATE OR REPLACE TABLE phases
        (name VARCHAR, start_ts TIMESTAMPTZ, end_ts TIMESTAMPTZ, created INT)""")
    con.executemany("INSERT INTO phases VALUES (?,?,?,?)", phases)
    con.execute("""CREATE OR REPLACE TABLE nodes AS
        SELECT count(DISTINCT node) AS n FROM fsync_snapshots""")

    # Cumulative -> per-window deltas, attributed to a phase by window midpoint.
    con.execute("""CREATE OR REPLACE VIEW deltas AS
        WITH d AS (
          SELECT node, map, comm, key, ts,
                 value - lag(value, 1, 0) OVER w AS delta,
                 lag(ts) OVER w AS prev_ts
          FROM fsync_snapshots
          WINDOW w AS (PARTITION BY node, map, comm, key ORDER BY ts)
        )
        SELECT d.*, p.name AS phase
        FROM d LEFT JOIN phases p
          ON prev_ts + (ts - prev_ts)/2 >= p.start_ts
         AND prev_ts + (ts - prev_ts)/2 <  p.end_ts
        WHERE prev_ts IS NOT NULL AND delta >= 0""")
    return summary


def report(con):
    print("== fsync-family blocked time by phase (all nodes summed) ==")
    dump(con, """
        SELECT coalesce(d.phase, 'outside-phases') AS phase, comm,
               round(sum(delta)/1e6, 2) AS blocked_s_total,
               round(sum(delta)/1e3 / greatest(1, any_value(p.created)), 1) AS blocked_ms_per_pod,
               round(sum(delta)/1e6
                     / ((SELECT n FROM nodes) * greatest(epoch(any_value(p.end_ts) - any_value(p.start_ts)), 0.001))
                     * 100, 1) AS pct_of_phase_per_node
        FROM deltas d LEFT JOIN phases p ON d.phase = p.name
        WHERE map = 'blocked_us_total'
        GROUP BY 1, 2 HAVING sum(delta) > 0 ORDER BY phase, blocked_s_total DESC
    """)

    # blocked_us_total is traced per-process only (no syscall key), so a mean
    # can only be reported per process, not per syscall. Aggregate calls by
    # process too so the mean's numerator and denominator cover the same
    # population; the per-syscall call split is in the next table.
    print("\n== fsync-family blocked time and mean per process (throughput phases) ==")
    dump(con, """
        WITH calls AS (
          SELECT comm, sum(delta) AS n FROM deltas
          WHERE map = 'calls' AND phase LIKE 'throughput%' GROUP BY 1
        ), blocked AS (
          SELECT comm, sum(delta) AS us FROM deltas
          WHERE map = 'blocked_us_total' AND phase LIKE 'throughput%' GROUP BY 1
        )
        SELECT b.comm, coalesce(c.n, 0) AS calls,
               round(b.us / 1e6, 2) AS blocked_s,
               round(b.us / nullif(c.n, 0), 0) AS mean_us_per_call
        FROM blocked b LEFT JOIN calls c USING (comm)
        WHERE b.us > 0 ORDER BY b.us DESC
    """)

    print("\n== fsync-family calls by process and syscall (throughput phases) ==")
    dump(con, """
        SELECT comm, key AS syscall, sum(delta) AS calls FROM deltas
        WHERE map = 'calls' AND phase LIKE 'throughput%'
        GROUP BY 1, 2 HAVING sum(delta) > 0 ORDER BY comm, calls DESC
    """)

    print("\n== top blocking syscalls >=1ms, waits excluded (throughput phase) ==")
    print("   (fsync/fdatasync must rank near the top for the fsync hypothesis to hold)")
    dump(con, f"""
        SELECT comm, key AS syscall,
               round(sum(CASE WHEN map = 'slow_syscall_us_total' THEN delta END)/1e6, 2)
                 AS blocked_s,
               sum(CASE WHEN map = 'slow_syscall_calls' THEN delta END) AS slow_calls
        FROM deltas
        WHERE map LIKE 'slow_syscall%' AND phase LIKE 'throughput%'
          AND key NOT IN ({','.join(f"'{s}'" for s in sorted(WAIT_SYSCALLS))})
        GROUP BY 1, 2 HAVING blocked_s > 0
        ORDER BY blocked_s DESC LIMIT 20
    """)

    print("\n== slow openat (>=1ms) blocked time by path bucket (throughput phase) ==")
    print("   (empty for runs whose profiler predates the openat classifier)")
    dump(con, """
        SELECT comm, key AS path_bucket,
               round(sum(CASE WHEN map = 'slow_open_us' THEN delta END)/1e6, 2) AS blocked_s,
               sum(CASE WHEN map = 'slow_open_calls' THEN delta END) AS slow_calls,
               round(sum(CASE WHEN map = 'slow_open_us' THEN delta END)/1e3
                     / nullif(sum(CASE WHEN map = 'slow_open_calls' THEN delta END), 0), 1)
                 AS mean_ms
        FROM deltas
        WHERE map LIKE 'slow_open%' AND phase LIKE 'throughput%'
        GROUP BY 1, 2 HAVING blocked_s > 0
        ORDER BY blocked_s DESC LIMIT 15
    """)

    print("\n== the same, waits included, for scale ==")
    dump(con, f"""
        SELECT comm, key AS syscall,
               round(sum(delta)/1e6, 2) AS blocked_s,
               key IN ({','.join(f"'{s}'" for s in sorted(WAIT_SYSCALLS))}) AS is_wait
        FROM deltas
        WHERE map = 'slow_syscall_us_total' AND phase LIKE 'throughput%'
        GROUP BY 1, 2 HAVING blocked_s > 1
        ORDER BY blocked_s DESC LIMIT 12
    """)


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("artifacts_dir")
    ap.add_argument("--db", default=":memory:",
                    help="persist parsed tables to this DuckDB file")
    args = ap.parse_args()

    con = duckdb.connect(args.db)
    summary = load(con, args.artifacts_dir)
    cluster = summary.get("cluster") or {}
    print(f"run {summary['runID']}: kubernetes {cluster.get('kubernetesVersion')}, "
          f"{cluster.get('nodes')} worker nodes\n")
    report(con)
    if args.db != ":memory:":
        print(f"\ntables persisted to {args.db} (fsync_snapshots, phases, deltas)")


if __name__ == "__main__":
    main()
