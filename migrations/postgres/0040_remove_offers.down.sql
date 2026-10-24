DO $$
BEGIN
    RAISE EXCEPTION 'ContentKit offer removal is irreversible. Restore a pre-migration backup to run an older ContentKit version; OpenRails remains the authoritative billing catalog.';
END
$$;
