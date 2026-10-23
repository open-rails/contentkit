-- Preserve domain cleanup before retiring the old callback transport. The old
-- callback could report success without a bound worker, so retain every purge
-- fact still present, including facts marked delivered.
CREATE TABLE content_event_purge_handoff (
    tenant_id text NOT NULL,
    event_id bigint NOT NULL,
    content_kind text NOT NULL CHECK (content_kind <> ''),
    content_id text NOT NULL CHECK (content_id <> ''),
    content_version_id text CHECK (content_version_id IS NULL OR content_version_id <> ''),
    PRIMARY KEY (tenant_id, event_id)
);
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM content_events WHERE type = 'version.purged'
               AND nullif(payload->>'version_id', '') IS NULL) THEN
        RAISE EXCEPTION 'contentkit: retained version purge fact lacks a version ID';
    END IF;
END $$;
INSERT INTO content_event_purge_handoff
    (tenant_id, event_id, content_kind, content_id, content_version_id)
SELECT tenant_id, id, payload->>'kind', payload->>'content_id',
       CASE WHEN type = 'version.purged' THEN payload->>'version_id' END
FROM content_events WHERE type IN ('content.purged', 'version.purged');

CREATE TABLE content_event_epoch (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    epoch uuid NOT NULL DEFAULT gen_random_uuid()
);
INSERT INTO content_event_epoch (singleton) VALUES (true);
CREATE TABLE content_event_streams (
    tenant_id text PRIMARY KEY,
    position bigint NOT NULL DEFAULT 0 CHECK (position >= 0),
    retained_after bigint NOT NULL DEFAULT 0 CHECK (retained_after >= 0 AND retained_after <= position),
    retained_boundary uuid
);
ALTER TABLE content_events ADD COLUMN position bigint;
ALTER TABLE content_events ADD COLUMN boundary uuid NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE content_events ADD COLUMN event_version integer NOT NULL DEFAULT 1 CHECK (event_version > 0);
WITH ordered AS (
    SELECT tenant_id, id, row_number() OVER (PARTITION BY tenant_id ORDER BY id) AS position
    FROM content_events
)
UPDATE content_events e SET position = o.position
FROM ordered o WHERE e.tenant_id = o.tenant_id AND e.id = o.id;
ALTER TABLE content_events ALTER COLUMN position SET NOT NULL;
ALTER TABLE content_events ADD CONSTRAINT content_events_position_positive CHECK (position > 0);
CREATE UNIQUE INDEX content_events_position ON content_events (tenant_id, position);
CREATE INDEX content_events_type_position ON content_events (tenant_id, type, position);
INSERT INTO content_event_streams (tenant_id, position)
SELECT tenant_id, max(position) FROM content_events GROUP BY tenant_id;
DROP INDEX content_events_undelivered_idx;
ALTER TABLE content_events DROP COLUMN delivered_at;
