-- parent: 10 sha256:ccd31a1dbe2e760bf90f374419b1a14bc821b1c9b9a2e601e9aad09eb0c794c3

-- Pending manifest writes and their effects, not a manifest head or file list.
-- An item cannot start another write until its prior outcome is fenced and
-- settled. Terminal identities remain so a caller retry cannot repeat an edit.
CREATE TABLE content_media_commits (
    tenant_id text NOT NULL,
    operation_id uuid NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    folder_prefix text NOT NULL,
    actor_id text NOT NULL,
    fingerprint bytea NOT NULL,
    lease_id uuid NOT NULL,
    attempt_id uuid,
    expected_etag text NOT NULL DEFAULT '',
    state text NOT NULL DEFAULT 'open',
    effects jsonb NOT NULL DEFAULT '{}'::jsonb,
    charged_bytes bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, operation_id),
    CHECK (tenant_id <> '' AND content_kind <> '' AND content_id <> '' AND folder_prefix <> ''),
    CHECK (octet_length(fingerprint) = 32),
    CHECK (operation_id <> '00000000-0000-0000-0000-000000000000' AND lease_id <> '00000000-0000-0000-0000-000000000000'),
    CHECK (state IN ('open', 'prepared', 'frozen', 'applied', 'absent')),
    CHECK (state <> 'prepared' OR attempt_id IS NOT NULL),
    CHECK (attempt_id IS NULL OR attempt_id <> '00000000-0000-0000-0000-000000000000'),
    CHECK (charged_bytes >= 0 AND jsonb_typeof(effects) = 'object')
);

CREATE UNIQUE INDEX content_media_commits_pending_folder ON content_media_commits (tenant_id, folder_prefix)
    WHERE state IN ('open', 'prepared', 'frozen');
CREATE INDEX content_media_commits_pending ON content_media_commits (tenant_id, updated_at, operation_id)
    WHERE state IN ('open', 'prepared', 'frozen');

-- Ownership is recorded before bytes are sent. Retirement is permanent;
-- cleanup may repeat, but a retired physical name cannot be adopted again.
CREATE TABLE content_media_allocations (
    tenant_id text NOT NULL,
    folder_prefix text NOT NULL,
    object_key text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    retired_at timestamptz,
    PRIMARY KEY (tenant_id, object_key),
    CHECK (tenant_id <> '' AND folder_prefix <> '' AND object_key <> '')
);
CREATE INDEX content_media_allocations_folder ON content_media_allocations (tenant_id, folder_prefix);
