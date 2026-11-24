-- New encode runs take time-ordered ids; existing ids are kept.
ALTER TABLE encode_run ALTER COLUMN id SET DEFAULT uuidv7();
