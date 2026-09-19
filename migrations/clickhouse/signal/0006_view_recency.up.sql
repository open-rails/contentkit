-- parent: 5 sha256:a97b38f508005b8245db2ab8f6a715a57885e3feaf7e714ad8265b851dd82beb
-- Watch history is ordered and cleared by consumption activity, not by later
-- clicks or feedback on the same content.

ALTER TABLE subject_content_state {{ON_CLUSTER}}
    ADD COLUMN IF NOT EXISTS last_view_at DateTime('UTC') DEFAULT toDateTime(0, 'UTC') AFTER last_signal_at;

-- Existing state rows predate last_view_at. Re-project the view timestamp from
-- canonical raw events before hosts start filtering or ordering by it. A new
-- ReplacingMergeTree version makes the backfill win over the copied row while
-- preserving every other compacted field exactly.
INSERT INTO subject_content_state (
    tenant, subject_kind, subject, content_kind, content_id, content_version_id,
    first_seen_at, last_signal_at, last_view_at, total_events, views,
    completions, active_s, max_progress, progress_max, completed, resume,
    last_score, net_value, feedback, version
)
SELECT
    old.tenant, old.subject_kind, old.subject, old.content_kind, old.content_id,
    old.content_version_id, old.first_seen_at, old.last_signal_at,
    if(view.has_view > 0, view.last_view_at, toDateTime(0, 'UTC')),
    old.total_events, old.views, old.completions, old.active_s, old.max_progress,
    old.progress_max, old.completed, old.resume, old.last_score, old.net_value,
    old.feedback, now64(6)
FROM (SELECT * FROM subject_content_state FINAL) AS old
LEFT JOIN (
    SELECT tenant, subject_kind, subject, content_kind, content_id,
           content_version_id,
           countIf(signal_type = 'view') AS has_view,
           maxIf(occurred_at, signal_type = 'view') AS last_view_at
    FROM (
        SELECT tenant, subject_kind, subject, content_kind, content_id,
               content_version_id, signal_type, event_id,
               argMax(occurred_at, version) AS occurred_at
        FROM signals FINAL
        GROUP BY tenant, subject_kind, subject, content_kind, content_id,
                 content_version_id, signal_type, event_id
    )
    GROUP BY tenant, subject_kind, subject, content_kind, content_id,
             content_version_id
) AS view
ON old.tenant = view.tenant
AND old.subject_kind = view.subject_kind
AND old.subject = view.subject
AND old.content_kind = view.content_kind
AND old.content_id = view.content_id
AND old.content_version_id = view.content_version_id;
