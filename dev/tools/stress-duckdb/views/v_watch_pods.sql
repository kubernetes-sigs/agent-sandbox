-- Pod watch events with common fields extracted from the K8s object.
CREATE OR REPLACE VIEW v_watch_pods AS
SELECT run_id,
       timestamp AS ts,
       type AS event_type,
       json_extract_string(object, '$.metadata.name') AS name,
       json_extract_string(object, '$.metadata.namespace') AS namespace,
       json_extract_string(object, '$.metadata.uid') AS uid,
       json_extract_string(object, '$.spec.nodeName') AS node_name,
       json_extract_string(object, '$.status.phase') AS phase,
       json_extract_string(object, '$.metadata.deletionTimestamp') AS deletion_timestamp,
       json_extract_string(object, '$.status.containerStatuses[0].containerID') AS container_id,
       json_extract_string(object, '$.status.containerStatuses[0].state.running.startedAt') AS container_started_at,
       object
FROM watch
WHERE resource = 'pods';
