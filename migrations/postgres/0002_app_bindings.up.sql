-- Ownership metadata maps new tenant/app identities to unchanged catalog keys.
-- It intentionally does not rewrite any existing content or access reference.
CREATE TABLE content_app_bindings (
    tenant_id uuid NOT NULL CHECK (tenant_id <> '00000000-0000-0000-0000-000000000000'),
    app_id uuid PRIMARY KEY CHECK (app_id <> '00000000-0000-0000-0000-000000000000'),
    catalog_key text NOT NULL UNIQUE CHECK (catalog_key ~ '^[a-z0-9_-]{1,64}$'),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX content_app_bindings_tenant_idx ON content_app_bindings (tenant_id, app_id);
