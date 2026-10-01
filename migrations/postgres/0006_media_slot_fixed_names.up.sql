-- parent: 5 sha256:835a4d224b1c63538c1abbe721e3c321918820d6afe2a76004f931511f15d2ee

-- A public slot's renditions moved to fixed public/ names. Clearing the
-- backfill record makes the host's media jobs write every existing slot's
-- fixed names once.
TRUNCATE content_media_slot_backfill;
