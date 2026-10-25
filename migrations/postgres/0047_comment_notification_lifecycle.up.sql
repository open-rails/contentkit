-- Source identities and revisions, not an account inbox: ContentKit retains
-- the exact notice keys it emits so comment deletion can withdraw them without
-- reading a recipient's inbox. The account owner stores payload/read state.
CREATE TABLE content_comment_notifications (
    tenant_id text NOT NULL,
    comment_id uuid NOT NULL,
    recipient text NOT NULL,
    source_id text NOT NULL,
    kind text NOT NULL,
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    withdrawn boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, recipient, source_id),
    FOREIGN KEY (tenant_id, comment_id) REFERENCES content_comments(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX content_comment_notifications_comment_idx
    ON content_comment_notifications (tenant_id, comment_id)
    WHERE NOT withdrawn;
