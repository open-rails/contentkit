
-- The slot index backfill, per tenant: until completed_at is set, the host's
-- media jobs index the slots that already exist, resuming after after_folder.
CREATE TABLE content_media_slot_backfill (
    tenant_id text PRIMARY KEY,
    after_folder text NOT NULL DEFAULT '',
    completed_at timestamptz
);
