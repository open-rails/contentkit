DO $$
BEGIN
    RAISE EXCEPTION 'ContentKit publishing removal is irreversible. Restore a pre-migration backup to run the previous combined-publishing version.';
END
$$;
