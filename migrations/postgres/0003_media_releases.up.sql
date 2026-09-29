-- parent: 2 sha256:bd648a43b9b0dd910088a117316c332cc967d82a2dc00b9992628aaada9e1bc2

CREATE TABLE content_media_releases (
    tenant_id text NOT NULL,
    operation_id text NOT NULL,
    folder_prefix text NOT NULL,
    owner_id text NOT NULL,
    bytes bigint NOT NULL CHECK (bytes >= 0),
    applied boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, operation_id)
);

CREATE UNIQUE INDEX content_media_releases_pending_folder
    ON content_media_releases (tenant_id, folder_prefix) WHERE NOT applied;
