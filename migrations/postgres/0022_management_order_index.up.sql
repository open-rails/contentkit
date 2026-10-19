-- Management lists include every state and order by creation time, then ID.
CREATE INDEX IF NOT EXISTS content_items_management_order_idx ON content_items
    (tenant_id, kind, created_at DESC, id DESC);
