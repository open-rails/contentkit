-- parent: 3 sha256:b8a3826ef9e32d2f84d126cc1a07959e5e066b9fd96e758ee09d091a9f8c583e

-- The slot index: one row per set slot whose renditions are in public/.
CREATE TABLE content_media_slots (
    tenant_id text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    slot text NOT NULL,
    aspect text NOT NULL,
    outputs jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, content_kind, content_id, slot)
);
