-- Deletion cost per run/phase (delete call -> pod observed gone).
CREATE OR REPLACE VIEW v_deletion AS
SELECT run_id, phase, count(*) AS n,
       round(quantile_cont(epoch(podDeleted::TIMESTAMPTZ - deleteCalled::TIMESTAMPTZ), 0.50), 2) AS delete_to_gone_p50_s,
       round(quantile_cont(epoch(podDeleted::TIMESTAMPTZ - deleteCalled::TIMESTAMPTZ), 0.90), 2) AS delete_to_gone_p90_s
FROM sandboxes WHERE podDeleted IS NOT NULL AND deleteCalled IS NOT NULL
GROUP BY 1, 2 ORDER BY run_id, phase;
