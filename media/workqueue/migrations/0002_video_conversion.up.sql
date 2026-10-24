-- Stop the previous worker fleet before this migration. One video job now owns
-- the conversion; retain completed ranges so it resumes rather than re-encodes.
INSERT INTO river_job (kind, args, queue, max_attempts, priority)
SELECT 'contentkit_media_video', jsonb_build_object('ref', r.ref, 'class',
    CASE r.priority WHEN 4 THEN 'backfill' WHEN 3 THEN 'reencode' ELSE '' END),
    'media_video', 32767, r.priority
FROM (SELECT ref, min(CASE class WHEN 'backfill' THEN 4 WHEN 'reencode' THEN 3 ELSE 1 END) AS priority
      FROM encode_run WHERE state IN ('planned', 'encoding', 'assembling') GROUP BY ref) r
WHERE NOT EXISTS (
    SELECT 1 FROM river_job j WHERE j.kind = 'contentkit_media_video'
      AND j.args->'ref' = r.ref AND COALESCE(j.args->>'preset', '') = ''
      AND j.state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
);

UPDATE river_job SET queue = 'media_video'
WHERE kind = 'contentkit_media_video'
  AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled');

UPDATE river_job SET state = 'cancelled', finalized_at = now()
WHERE kind IN ('contentkit_media_video_chunk', 'contentkit_media_video_assemble')
  AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled');

UPDATE encode_chunk SET state = 'planned' WHERE state = 'queued';
ALTER TABLE encode_chunk DROP COLUMN job_id, DROP CONSTRAINT encode_chunk_state_check;
ALTER TABLE encode_chunk ADD CHECK (state IN ('planned', 'done'));
ALTER TABLE encode_run DROP COLUMN released, DROP COLUMN completed;
