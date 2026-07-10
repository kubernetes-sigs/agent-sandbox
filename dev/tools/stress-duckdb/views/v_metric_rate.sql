-- Generic counter -> rate/s: filter by metric before selecting from this view.
CREATE OR REPLACE VIEW v_metric_rate AS
SELECT run_id, source, instance, metric, labels, ts,
       (value - lag(value) OVER w) / greatest(epoch(ts::TIMESTAMPTZ - lag(ts::TIMESTAMPTZ) OVER w), 0.001) AS rate_per_s
FROM metrics
WINDOW w AS (PARTITION BY run_id, source, instance, metric, CAST(labels AS VARCHAR) ORDER BY ts);
