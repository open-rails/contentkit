-- The media worker's video conversions, in its schema beside River's tables.
-- A run is one rendition of one source; its chunks are the ranges already
-- encoded, so an interrupted conversion resumes rather than re-encodes.
CREATE TABLE encode_run (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id text NOT NULL,
    ref jsonb NOT NULL,
    file_name text NOT NULL,
    source_name text NOT NULL,
    source_key text NOT NULL,
    source_etag text NOT NULL,
    spec text NOT NULL,
    rung integer NOT NULL CHECK (rung > 0),
    class text NOT NULL,
    probe jsonb NOT NULL,
    passthrough_codec text NOT NULL DEFAULT '' CHECK (passthrough_codec IN ('', 'h264', 'hevc')),
    state text NOT NULL DEFAULT 'planned' CHECK (state IN ('planned', 'encoding', 'assembling', 'complete', 'cancelled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (ref, file_name, source_name, source_etag, spec, rung)
);
CREATE INDEX encode_run_active_tenant_idx ON encode_run (tenant_id, state)
    WHERE state IN ('planned', 'encoding', 'assembling');

CREATE TABLE encode_chunk (
    run_id uuid NOT NULL REFERENCES encode_run(id) ON DELETE CASCADE,
    ordinal integer NOT NULL,
    start_ms bigint NOT NULL,
    end_ms bigint NOT NULL,
    state text NOT NULL DEFAULT 'planned' CHECK (state IN ('planned', 'done')),
    output jsonb,
    finished_at timestamptz,
    PRIMARY KEY (run_id, ordinal),
    CHECK (ordinal >= 0 AND start_ms >= 0 AND end_ms > start_ms)
);
CREATE INDEX encode_chunk_pending_idx ON encode_chunk (run_id, ordinal)
    WHERE state = 'planned';
