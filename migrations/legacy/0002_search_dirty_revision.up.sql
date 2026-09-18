-- parent: 1 sha256:087b205cb4786bd7b1603b019b4ede03eae4b974d9626d29166bed2e3033aae8
-- A timestamp is not a queue generation: now() is constant within a host
-- transaction, and delete/reinsert must not reuse an earlier generation.
ALTER TABLE search_dirty ADD COLUMN revision bigserial;

CREATE FUNCTION searchkit_bump_dirty_revision() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.revision := nextval(pg_get_serial_sequence(
        format('%I.%I', TG_TABLE_SCHEMA, TG_TABLE_NAME), 'revision'));
    RETURN NEW;
END;
$$;

-- Override explicit copies as well as ordinary host UPSERTs. Gaps are harmless.
CREATE TRIGGER search_dirty_revision
BEFORE INSERT OR UPDATE ON search_dirty
FOR EACH ROW EXECUTE FUNCTION searchkit_bump_dirty_revision();
