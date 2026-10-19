-- Newest and oldest lists read content in this order and stop at the page.
-- Not partial: the listing predicate also admits deleted rows to their holders.
CREATE INDEX IF NOT EXISTS content_facets_published_order_idx ON content_facets
    (tenant_id, kind, coalesce(published_at, 'epoch'::timestamptz) DESC, content_id DESC);
