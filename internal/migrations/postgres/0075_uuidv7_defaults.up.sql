-- New rows take time-ordered ids, so inserts append to their indexes.
-- Existing ids are kept.
ALTER TABLE content_comments ALTER COLUMN id SET DEFAULT uuidv7();
ALTER TABLE content_poll_options ALTER COLUMN id SET DEFAULT uuidv7();
ALTER TABLE content_poll_questions ALTER COLUMN id SET DEFAULT uuidv7();
ALTER TABLE content_poll_votes ALTER COLUMN id SET DEFAULT uuidv7();
ALTER TABLE content_publication_requests ALTER COLUMN id SET DEFAULT uuidv7();
ALTER TABLE content_segments ALTER COLUMN id SET DEFAULT uuidv7()::text;
