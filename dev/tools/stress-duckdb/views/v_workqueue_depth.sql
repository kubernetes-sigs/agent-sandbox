-- All workqueue depths across components.
CREATE OR REPLACE VIEW v_workqueue_depth AS
SELECT run_id, ts, source, instance, labels['name'] AS queue, value AS depth
FROM metrics WHERE metric = 'workqueue_depth'
ORDER BY run_id, ts;
