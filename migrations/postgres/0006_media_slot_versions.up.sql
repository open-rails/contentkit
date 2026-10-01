-- parent: 5 sha256:835a4d224b1c63538c1abbe721e3c321918820d6afe2a76004f931511f15d2ee

-- A public slot's renditions moved to fixed public/ names; its URLs carry the
-- slot's version (?v=). Clearing the backfill record makes the host's media
-- jobs write every existing slot's fixed copies and versioned row once.
ALTER TABLE content_media_slots ADD COLUMN version text NOT NULL DEFAULT '';
TRUNCATE content_media_slot_backfill;
