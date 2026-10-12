
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
