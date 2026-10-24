-- Original audio attachments are played directly. Old workers must be stopped
-- before upgrading; retain terminal history, but never run a retired conversion.
-- The existing ingest worker also relays ready metadata to the host. Give each
-- interrupted audio item that pass so its original gains a playback snapshot.
INSERT INTO river_job (kind, args, queue, max_attempts)
SELECT 'contentkit_media_ingest', jsonb_build_object('ref', r.ref), 'media_ingest', 5
FROM (
    SELECT DISTINCT args->'ref' AS ref FROM river_job
    WHERE kind = 'contentkit_media_audio'
      AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
      AND jsonb_typeof(args->'ref') = 'object'
) r
WHERE NOT EXISTS (
    SELECT 1 FROM river_job j
    WHERE j.kind = 'contentkit_media_ingest' AND j.args->'ref' = r.ref
      AND j.state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
);

UPDATE river_job SET state = 'cancelled', finalized_at = now()
WHERE kind = 'contentkit_media_audio'
  AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled');
