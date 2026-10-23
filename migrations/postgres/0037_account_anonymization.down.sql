-- Anonymous rows cannot regain their former account association.
DO $$ BEGIN RAISE EXCEPTION 'contentkit: account anonymization requires a forward migration'; END $$;
