UPDATE river_job SET max_attempts = 5
WHERE kind = 'contentkit_media_video' AND max_attempts = 32767
  AND state IN ('available', 'scheduled', 'retryable', 'pending', 'running');
UPDATE river_job SET queue = 'media_video'
WHERE queue = 'media_video_light' AND kind = 'contentkit_media_video'
  AND state IN ('available', 'scheduled', 'retryable', 'pending', 'running');
DROP TABLE encode_chunk;
DROP TABLE encode_run;
