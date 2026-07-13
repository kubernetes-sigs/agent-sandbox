-- Launch latency per run/phase from per-sandbox records.
CREATE OR REPLACE VIEW v_launch_latency AS
SELECT run_id, phase, count(*) AS n,
       round(quantile_cont(sandboxReadyMs, 0.50), 0) AS e2e_p50_ms,
       round(quantile_cont(sandboxReadyMs, 0.90), 0) AS e2e_p90_ms,
       round(quantile_cont(sandboxReadyMs, 0.99), 0) AS e2e_p99_ms
FROM sandboxes WHERE sandboxReadyMs IS NOT NULL
GROUP BY 1, 2 ORDER BY run_id, phase;
