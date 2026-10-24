-- Old producers and workers must be stopped. Preserve each queued request's
-- identity/history while routing placement through the combined ingestion job.
UPDATE river_job
SET kind='contentkit_media_ingest',queue='media_ingest',unique_key=NULL,unique_states=NULL
WHERE kind='contentkit_media_place'
  AND state IN ('available','pending','retryable','running','scheduled');
