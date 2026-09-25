-- parent: 1 sha256:ef8a0834cf3f1125e4002ca70e3890248a7da3d7a2ca54f4aa09220b98ccd076
-- Existing plan jobs were queued with the old five-attempt River ceiling.
-- The worker counts actual failures; River rescue of a hard-killed pod must
-- not discard the job before it can restore its attempt in Work.
UPDATE river_job SET max_attempts = 32767
WHERE kind = 'contentkit_media_video' AND max_attempts < 32767
  AND state IN ('available', 'scheduled', 'retryable', 'pending', 'running');
