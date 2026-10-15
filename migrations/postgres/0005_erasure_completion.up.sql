-- The source fence precedes external/signal cleanup. A restore may not serve
-- merely because that fence exists while the coordinated cleanup is pending.
ALTER TABLE content_erased_subjects
    ADD COLUMN engine_cleanup_complete boolean NOT NULL DEFAULT false;
