DROP TABLE encode_chunk;
DROP TABLE encode_run;
UPDATE river_job SET queue = 'media_video'
WHERE queue = 'media_video_light' AND kind = 'contentkit_media_video'
  AND state IN ('available', 'scheduled', 'retryable', 'pending', 'running');
