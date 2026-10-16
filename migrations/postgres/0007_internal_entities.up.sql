ALTER TABLE content_groups ADD COLUMN internal boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN content_groups.internal IS
  'Internal classification marker: usable by domain rules but absent from public metadata and discovery.';
