-- Tenant-owned media journal, allocations, publications and quota state.
CREATE TABLE IF NOT EXISTS content_media_commits (
    tenant_id text NOT NULL,
    operation_id uuid NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    folder_prefix text NOT NULL,
    actor_id text NOT NULL,
    fingerprint bytea NOT NULL,
    lease_id uuid NOT NULL,
    attempt_id uuid,
    expected_etag text DEFAULT ''::text NOT NULL,
    state text DEFAULT 'open'::text NOT NULL,
    effects jsonb DEFAULT '{}'::jsonb NOT NULL,
    charged_bytes bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    content_version_id text,
    CONSTRAINT content_media_commits_pkey PRIMARY KEY (tenant_id, operation_id),
    CONSTRAINT content_media_commits_attempt_id_check CHECK (((attempt_id IS NULL) OR (attempt_id <> '00000000-0000-0000-0000-000000000000'::uuid))),
    CONSTRAINT content_media_commits_check CHECK (((tenant_id <> ''::text) AND (content_kind <> ''::text) AND (content_id <> ''::text) AND (folder_prefix <> ''::text))),
    CONSTRAINT content_media_commits_check1 CHECK (((operation_id <> '00000000-0000-0000-0000-000000000000'::uuid) AND (lease_id <> '00000000-0000-0000-0000-000000000000'::uuid))),
    CONSTRAINT content_media_commits_check2 CHECK (((state <> 'prepared'::text) OR (attempt_id IS NOT NULL))),
    CONSTRAINT content_media_commits_check3 CHECK (((charged_bytes >= 0) AND (jsonb_typeof(effects) = 'object'::text))),
    CONSTRAINT content_media_commits_fingerprint_check CHECK ((octet_length(fingerprint) = 32)),
    CONSTRAINT content_media_commits_state_check CHECK ((state = ANY (ARRAY['open'::text, 'prepared'::text, 'frozen'::text, 'applied'::text, 'absent'::text]))),
    CONSTRAINT content_media_commits_version_check CHECK ((content_version_id <> ''::text))
);

CREATE INDEX IF NOT EXISTS content_media_commits_pending ON content_media_commits USING btree (tenant_id, updated_at, operation_id) WHERE (state = ANY (ARRAY['open'::text, 'prepared'::text, 'frozen'::text]));
CREATE UNIQUE INDEX IF NOT EXISTS content_media_commits_pending_folder ON content_media_commits USING btree (tenant_id, folder_prefix) WHERE (state = ANY (ARRAY['open'::text, 'prepared'::text, 'frozen'::text]));

CREATE TABLE IF NOT EXISTS content_media_allocations (
    tenant_id text NOT NULL,
    folder_prefix text NOT NULL,
    object_key text NOT NULL,
    incarnation uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    retired_at timestamp with time zone,
    CONSTRAINT content_media_allocations_pkey PRIMARY KEY (tenant_id, object_key),
    CONSTRAINT content_media_allocations_check CHECK (((tenant_id <> ''::text) AND (folder_prefix <> ''::text) AND (object_key <> ''::text)))
);

CREATE INDEX IF NOT EXISTS content_media_allocations_folder ON content_media_allocations USING btree (tenant_id, folder_prefix);

CREATE TABLE IF NOT EXISTS content_media_publications (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    upload_name text NOT NULL,
    upload_path text NOT NULL,
    preset text NOT NULL,
    ordinal integer NOT NULL,
    generation uuid,
    renditions jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    content_version_id text,
    CONSTRAINT content_media_publications_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, upload_name, upload_path, preset),
    CONSTRAINT content_media_publications_check CHECK (((tenant_id <> ''::text) AND (content_kind <> ''::text) AND (content_id <> ''::text) AND (upload_path <> ''::text) AND (preset <> ''::text))),
    CONSTRAINT content_media_publications_check1 CHECK (((ordinal >= 0) AND (jsonb_typeof(renditions) = 'array'::text) AND (jsonb_array_length(renditions) > 0))),
    CONSTRAINT content_media_publications_version_check CHECK ((content_version_id <> ''::text))
);

CREATE TABLE IF NOT EXISTS content_media_reservations (
    tenant_id text NOT NULL,
    object_key text NOT NULL,
    owner_id text NOT NULL,
    bytes bigint NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT content_media_reservations_pkey PRIMARY KEY (tenant_id, object_key)
);

CREATE INDEX IF NOT EXISTS content_media_reservations_owner ON content_media_reservations USING btree (tenant_id, owner_id, expires_at);

CREATE TABLE IF NOT EXISTS content_media_releases (
    tenant_id text NOT NULL,
    operation_id text NOT NULL,
    folder_prefix text NOT NULL,
    owner_id text NOT NULL,
    bytes bigint NOT NULL,
    applied boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_media_releases_pkey PRIMARY KEY (tenant_id, operation_id),
    CONSTRAINT content_media_releases_bytes_check CHECK ((bytes >= 0))
);

CREATE UNIQUE INDEX IF NOT EXISTS content_media_releases_pending_folder ON content_media_releases USING btree (tenant_id, folder_prefix) WHERE (NOT applied);

CREATE TABLE IF NOT EXISTS content_media_usage (
    tenant_id text NOT NULL,
    owner_id text NOT NULL,
    used_bytes bigint DEFAULT 0 NOT NULL,
    CONSTRAINT content_media_usage_pkey PRIMARY KEY (tenant_id, owner_id)
);

CREATE TABLE IF NOT EXISTS content_media_upload_rates (
    tenant_id text NOT NULL,
    uploader text NOT NULL,
    hour timestamp with time zone NOT NULL,
    files integer NOT NULL,
    bytes bigint NOT NULL,
    CONSTRAINT content_media_upload_rates_pkey PRIMARY KEY (tenant_id, uploader, hour)
);

CREATE TABLE IF NOT EXISTS content_media_upgrades (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    after_id text DEFAULT ''::text NOT NULL,
    visited bigint DEFAULT 0 NOT NULL,
    upgraded bigint DEFAULT 0 NOT NULL,
    finished_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_media_upgrades_pkey PRIMARY KEY (tenant_id, content_kind),
    CONSTRAINT content_media_upgrades_check CHECK (((tenant_id <> ''::text) AND (content_kind <> ''::text) AND (visited >= 0) AND (upgraded >= 0)))
);

CREATE TABLE IF NOT EXISTS content_media_upgrade_failures (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    error text NOT NULL,
    failed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_media_upgrade_failures_pkey PRIMARY KEY (tenant_id, content_kind, content_id)
);
