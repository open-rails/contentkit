-- Reconstructing a legacy writer while shared writers are live is unsafe.
DO $$ BEGIN RAISE EXCEPTION 'contentkit: shared event storage rollback requires a coordinated forward migration'; END $$;
