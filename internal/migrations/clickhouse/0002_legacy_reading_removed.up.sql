ALTER TABLE legacy_reading_state {{ON_CLUSTER}} ADD COLUMN IF NOT EXISTS removed Bool DEFAULT false;
