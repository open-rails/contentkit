UPDATE river_job SET max_attempts = 5
WHERE kind = 'contentkit_media_video' AND max_attempts = 32767
  AND state IN ('available', 'scheduled', 'retryable', 'pending', 'running');
