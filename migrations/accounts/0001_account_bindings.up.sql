-- Shared account storage is claimed by one business, independently of which
-- app's catalog schema hosts a runtime. Existing account paths stay unchanged.
CREATE TABLE content_account_bindings (
    account_namespace text PRIMARY KEY CHECK (account_namespace <> ''),
    tenant_id uuid NOT NULL UNIQUE CHECK (tenant_id <> '00000000-0000-0000-0000-000000000000'),
    created_at timestamptz NOT NULL DEFAULT now()
);
