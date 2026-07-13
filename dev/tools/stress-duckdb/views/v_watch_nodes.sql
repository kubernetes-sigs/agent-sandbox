-- Node watch events with capacity / condition fields extracted.
CREATE OR REPLACE VIEW v_watch_nodes AS
SELECT run_id,
       timestamp AS ts,
       type AS event_type,
       json_extract_string(object, '$.metadata.name') AS name,
       json_extract_string(object, '$.metadata.uid') AS uid,
       json_extract_string(object, '$.status.capacity.pods') AS capacity_pods,
       json_extract_string(object, '$.status.allocatable.pods') AS allocatable_pods,
       json_extract_string(object, '$.status.nodeInfo.kubeletVersion') AS kubelet_version,
       json_extract(object, '$.status.conditions') AS conditions,
       json_extract(object, '$.metadata.labels') AS labels,
       object
FROM watch
WHERE resource = 'nodes';
