-- Phase wall-clock windows (phases run sequentially from startTime).
CREATE OR REPLACE VIEW phases AS
WITH r AS (
  SELECT run_id, startTime::TIMESTAMPTZ AS t0,
         coalesce(phases.fill.durationSeconds, 0) AS d_fill,
         coalesce(phases.probe.durationSeconds, 0) AS d_probe,
         coalesce(phases.throughput.durationSeconds, 0) AS d_tp
  FROM runs_raw)
SELECT run_id, 'fill' AS phase, t0 AS start_ts, t0 + d_fill * INTERVAL 1 SECOND AS end_ts FROM r WHERE d_fill > 0
UNION ALL
SELECT run_id, 'probe', t0 + d_fill * INTERVAL 1 SECOND, t0 + (d_fill + d_probe) * INTERVAL 1 SECOND FROM r WHERE d_probe > 0
UNION ALL
SELECT run_id, 'throughput', t0 + (d_fill + d_probe) * INTERVAL 1 SECOND, t0 + (d_fill + d_probe + d_tp) * INTERVAL 1 SECOND FROM r WHERE d_tp > 0;
