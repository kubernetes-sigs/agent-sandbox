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

"""Generate the fleet run report: one self-contained HTML page.

Reads the fleet driver's artifacts (creates-*.jsonl.gz, ready-*.jsonl.gz,
summary.json) and renders:
  - verdict stat tiles (best 60s server-clock window vs the 1M/min target)
  - an aggregate created/s vs ready/s timeline
  - per-cluster ready-rate small multiples
  - a per-cluster table (the data view for the charts)

The raw jsonl is the source of truth: the verdict here is recomputed from
the ready rows' server-side stamps, and a mismatch against summary.json is
rendered as a loud warning banner, not silently reconciled.
"""

import argparse
import glob
import json
import os

import duckdb
import jinja2
import markupsafe

TEMPLATE = os.path.join(os.path.dirname(__file__), "template.html.j2")


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--input-dir", required=True)
    p.add_argument("--output", required=True)
    args = p.parse_args()

    con = duckdb.connect()
    con.execute("SET TimeZone='UTC'")

    creates_glob = os.path.join(args.input_dir, "creates-*.jsonl.gz")
    ready_glob = os.path.join(args.input_dir, "ready-*.jsonl.gz")
    if not glob.glob(creates_glob) or not glob.glob(ready_glob):
        raise SystemExit(f"no fleet artifacts under {args.input_dir}")

    with open(os.path.join(args.input_dir, "summary.json")) as f:
        summary = json.load(f)

    # t0 = first create (client clock, driver-wide).
    t0 = con.execute(
        f"SELECT min(created::TIMESTAMPTZ) FROM read_json_auto('{creates_glob}')"
    ).fetchone()[0]

    # Aggregate per-second created and ready rates. Server-side ready
    # stamps have 1s granularity, which matches this bucketing exactly.
    created_series = con.execute(f"""
        SELECT date_diff('second', TIMESTAMPTZ '{t0}', created::TIMESTAMPTZ) AS t, count(*) AS n
        FROM read_json_auto('{creates_glob}') GROUP BY t ORDER BY t""").fetchall()
    ready_series = con.execute(f"""
        SELECT date_diff('second', TIMESTAMPTZ '{t0}', serverReady::TIMESTAMPTZ) AS t, count(*) AS n
        FROM read_json_auto('{ready_glob}') GROUP BY t ORDER BY t""").fetchall()

    per_cluster_ready = con.execute(f"""
        SELECT cluster, date_diff('second', TIMESTAMPTZ '{t0}', serverReady::TIMESTAMPTZ) AS t, count(*) AS n
        FROM read_json_auto('{ready_glob}') GROUP BY cluster, t ORDER BY cluster, t""").fetchall()

    cluster_stats = con.execute(f"""
        WITH c AS (
          SELECT cluster, count(*) AS created,
                 round(quantile_cont(ackMs, 0.5), 1) AS ack_p50,
                 round(quantile_cont(ackMs, 0.99), 1) AS ack_p99
          FROM read_json_auto('{creates_glob}') GROUP BY cluster),
        r AS (
          SELECT cluster, count(*) AS ready,
                 date_diff('second', TIMESTAMPTZ '{t0}', min(serverReady::TIMESTAMPTZ)) AS first_ready_s,
                 date_diff('second', TIMESTAMPTZ '{t0}', max(serverReady::TIMESTAMPTZ)) AS last_ready_s
          FROM read_json_auto('{ready_glob}') GROUP BY cluster)
        SELECT c.cluster, c.created, coalesce(r.ready, 0), c.ack_p50, c.ack_p99,
               r.first_ready_s, r.last_ready_s
        FROM c LEFT JOIN r ON c.cluster = r.cluster ORDER BY c.cluster""").fetchall()

    # Recompute both windows from raw rows (source of truth). Fixed
    # [firstReady+10s, +70s) is the honest headline; sliding is reference.
    best60 = con.execute(f"""
        WITH s AS (SELECT serverReady::TIMESTAMPTZ AS ts FROM read_json_auto('{ready_glob}'))
        SELECT max(cnt) FROM (
          SELECT count(*) OVER (
            ORDER BY ts RANGE BETWEEN CURRENT ROW AND INTERVAL 60 SECONDS FOLLOWING
          ) AS cnt FROM s)""").fetchone()[0] or 0
    fixed60 = con.execute(f"""
        WITH s AS (SELECT serverReady::TIMESTAMPTZ AS ts FROM read_json_auto('{ready_glob}')),
        w AS (SELECT min(ts) + INTERVAL 10 SECONDS AS lo FROM s)
        SELECT count(*) FROM s, w WHERE ts >= w.lo AND ts < w.lo + INTERVAL 60 SECONDS""").fetchone()[0] or 0

    verdict_mismatch = fixed60 != summary.get("fixedWindow60s")

    def series_json(rows):
        return [[int(t), int(n)] for t, n in rows if t is not None]

    clusters = sorted({row[0] for row in per_cluster_ready})
    per_cluster = {
        name: series_json([(t, n) for c, t, n in per_cluster_ready if c == name])
        for name in clusters
    }

    env = jinja2.Environment(
        loader=jinja2.FileSystemLoader(os.path.dirname(TEMPLATE)),
        autoescape=True,
    )
    def js(v):
        # Inline-JSON for a <script> block: autoescape would HTML-escape
        # the quotes (a JS syntax error that silently kills every chart),
        # so these are marked safe after neutralizing '<' (script-close).
        return markupsafe.Markup(json.dumps(v).replace("<", "\\u003c"))

    html = env.get_template(os.path.basename(TEMPLATE)).render(
        summary=summary,
        best60=best60,
        fixed60=fixed60,
        verdict_mismatch=verdict_mismatch,
        created_series=js(series_json(created_series)),
        ready_series=js(series_json(ready_series)),
        per_cluster=js(per_cluster),
        cluster_stats=cluster_stats,
        input_dir=os.path.abspath(args.input_dir),
    )
    # Self-check: an HTML entity inside the script block means autoescape
    # mangled the inline JSON - a silent all-charts-dead failure mode.
    script = html.split("<script>", 1)[1].split("</script>", 1)[0]
    if "&#" in script or "&amp;" in script:
        raise SystemExit("BUG: HTML-escaped content inside <script>; charts would not render")

    with open(args.output, "w") as f:
        f.write(html)
    print(f"wrote {args.output} (best60={best60}, clusters={len(clusters)})")


if __name__ == "__main__":
    main()
