CREATE TABLE content_import_recounts (
    tenant_id text NOT NULL,
    target text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    content_version_id text,
    mark bigint DEFAULT 1 NOT NULL,
    marked_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_import_recounts_target_check CHECK ((target = ANY (ARRAY['counts'::text, 'replies'::text, 'votes'::text]))),
    CONSTRAINT content_import_recounts_key UNIQUE NULLS NOT DISTINCT (tenant_id, target, content_kind, content_id, content_version_id)
);

CREATE INDEX content_import_recounts_marked_idx ON content_import_recounts USING btree (tenant_id, marked_at);
