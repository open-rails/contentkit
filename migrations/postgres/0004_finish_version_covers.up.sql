-- Old content-level cover metadata may be cleared only after its files and
-- metadata have been copied and verified by the explicit media migration.
-- Refuse rather than silently dropping a cover or inventing a version owner.
DO $$
DECLARE
    pending boolean;
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'content_items' AND column_name = 'cover'
    ) THEN
        EXECUTE 'SELECT EXISTS (SELECT 1 FROM content_items WHERE cover IS NOT NULL)' INTO pending;
        IF pending THEN
            RAISE EXCEPTION 'contentkit: legacy covers require an explicit verified media migration before schema cleanup';
        END IF;
        ALTER TABLE content_items DROP COLUMN cover;
    END IF;
END;
$$;
