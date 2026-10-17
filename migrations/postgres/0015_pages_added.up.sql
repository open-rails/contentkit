-- When a republication last added pages to a version (new chapters in the
-- same edition), so cards can mark the work updated.
ALTER TABLE content_versions ADD COLUMN IF NOT EXISTS pages_added_at timestamptz;
