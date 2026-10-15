-- Billing provenance is a verified tenant association, not a visibility label.
CREATE TABLE content_notification_merchants (
    tenant_id uuid PRIMARY KEY REFERENCES content_account_bindings(tenant_id),
    source_system text NOT NULL CHECK (source_system <> ''),
    merchant_id text NOT NULL CHECK (merchant_id <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source_system, merchant_id)
);
