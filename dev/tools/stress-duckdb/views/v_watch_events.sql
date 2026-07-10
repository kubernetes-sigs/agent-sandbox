-- Kubernetes Event objects from the watch stream.
CREATE OR REPLACE VIEW v_watch_events AS
SELECT run_id,
       timestamp AS ts,
       type AS event_type,
       json_extract_string(object, '$.metadata.namespace') AS namespace,
       json_extract_string(object, '$.involvedObject.kind') AS involved_kind,
       json_extract_string(object, '$.involvedObject.name') AS involved_name,
       json_extract_string(object, '$.reason') AS reason,
       json_extract_string(object, '$.type') AS k8s_type,
       json_extract_string(object, '$.message') AS message,
       json_extract_string(object, '$.count') AS count,
       object
FROM watch
WHERE resource = 'events';
