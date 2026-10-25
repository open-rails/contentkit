-- Durable erasure intent must survive a content-only restore independently
-- of optional AuthKit inbox storage. Empty app_id means explicit whole-account
-- erasure; a UUID means only that app's erasure/anonymization obligation.
CREATE TABLE content_account_erasure_intents (
    tenant_id uuid NOT NULL REFERENCES content_account_bindings(tenant_id),
    app_id text NOT NULL CHECK (CASE WHEN app_id = '' THEN true ELSE
        app_id::uuid <> '00000000-0000-0000-0000-000000000000'::uuid
        AND app_id::uuid::text = app_id END),
    actor_id text NOT NULL CHECK (actor_id <> ''),
    requested_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, app_id, actor_id)
);

-- Completion receipts also represent ordinary account anonymization. They
-- prove an obligation only for their recorded app, not destructive intent
-- across every app. Existing AuthKit global fences remain authoritative.
INSERT INTO content_account_erasure_intents (tenant_id, app_id, actor_id, requested_at)
SELECT tenant_id, app_id::text, actor_id, completed_at
FROM content_account_erasure_receipts;
