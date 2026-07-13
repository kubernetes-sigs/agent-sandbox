-- The KCM garbage collector queue: the suspected churn-throughput ceiling.
CREATE OR REPLACE VIEW v_gc_queue_depth AS
SELECT run_id, ts, instance, labels['name'] AS queue, value AS depth
FROM metrics
WHERE source = 'kube-controller-manager' AND metric = 'workqueue_depth'
  AND labels['name'] LIKE 'garbage_collector%'
ORDER BY run_id, ts;
