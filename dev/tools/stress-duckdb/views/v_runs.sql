-- Headline numbers per run: the trend table.
CREATE OR REPLACE VIEW v_runs AS
SELECT run_id,
       startTime::TIMESTAMPTZ AS run_ts,
       cluster.kubernetesVersion AS k8s,
       cluster.nodes AS worker_nodes,
       config.maxInFlight AS max_in_flight,
       phases.probe.latency.endToEndReady.p50Ms AS probe_e2e_p50_ms,
       phases.probe.latency.endToEndReady.p90Ms AS probe_e2e_p90_ms,
       phases.probe.latency.scheduledToPodRunning.p50Ms AS probe_sched_to_running_p50_ms,
       phases.throughput.readyThroughput.steadyStatePerSecond AS tp_ready_steady_per_s,
       phases.throughput.readyThroughputPerNode.steadyStatePerSecond AS tp_ready_steady_per_node_per_s,
       phases.throughput.latency.endToEndReady.p50Ms AS tp_e2e_p50_ms,
       phases.fill.durationSeconds AS fill_duration_s
FROM runs_raw ORDER BY run_ts;
