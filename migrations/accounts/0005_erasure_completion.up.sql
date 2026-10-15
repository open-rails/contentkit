-- Full account cleanup survives catalog-only restores. Local app replay must
-- never discharge unfinished shared-avatar or account cleanup obligations.
CREATE TABLE content_account_erasure_receipts (
    tenant_id uuid NOT NULL REFERENCES content_account_bindings(tenant_id),
    app_id uuid NOT NULL CHECK (app_id <> '00000000-0000-0000-0000-000000000000'::uuid),
    actor_id text NOT NULL CHECK (actor_id <> ''),
    completed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, app_id, actor_id)
);
