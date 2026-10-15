-- A primary app is deployment placement, not the owner of account avatars.
-- The tenant owns this record and all account media state.
CREATE TABLE content_account_avatar_placement (
    tenant_id uuid PRIMARY KEY REFERENCES content_account_bindings(tenant_id),
    primary_app_id uuid NOT NULL CHECK (primary_app_id <> '00000000-0000-0000-0000-000000000000'::uuid),
    created_at timestamptz NOT NULL DEFAULT now()
);
