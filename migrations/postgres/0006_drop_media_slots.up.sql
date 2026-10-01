-- parent: 5 sha256:835a4d224b1c63538c1abbe721e3c321918820d6afe2a76004f931511f15d2ee

-- One media model (#101): public files live at deterministic names, so the
-- slot index and its backfill go. No table records what media exists.
DROP TABLE content_media_slot_backfill;
DROP TABLE content_media_slots;
