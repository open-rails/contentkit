-- parent: root
CREATE TABLE encode_run (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
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
    state text NOT NULL DEFAULT 'planned' CHECK (state IN ('planned', 'encoding', 'assembling', 'complete', 'cancelled')),
    released integer NOT NULL DEFAULT 0,
    completed integer NOT NULL DEFAULT 0,
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
    state text NOT NULL DEFAULT 'planned' CHECK (state IN ('planned', 'queued', 'done')),
    job_id bigint,
    output jsonb,
    finished_at timestamptz,
    PRIMARY KEY (run_id, ordinal),
    CHECK (ordinal >= 0 AND start_ms >= 0 AND end_ms > start_ms)
);

CREATE INDEX encode_chunk_pending_idx ON encode_chunk (run_id, ordinal)
    WHERE state = 'planned';

-- The kind is unchanged, so jobs persisted before the queue split keep their
-- arguments and run on the new light queue after the worker is upgraded.
UPDATE river_job SET queue = 'media_video_light'
WHERE queue = 'media_video' AND kind = 'contentkit_media_video'
  AND state IN ('available', 'scheduled', 'retryable', 'pending', 'running');
