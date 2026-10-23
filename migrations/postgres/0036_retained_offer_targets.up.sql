-- Offers and billing history outlive their physical content. Keep the parent
-- identity needed to recover a version product's key after the version is gone.
ALTER TABLE content_offers ADD COLUMN content_id text;
UPDATE content_offers o SET content_id = CASE
    WHEN target_kind = 'version' THEN (
        SELECT v.content_id FROM content_versions v
        WHERE v.tenant_id = o.tenant_id AND v.id = o.target_id)
    END;

-- Repair offers whose content was already deleted before this policy change.
-- Their unsynced revision is the durable obligation to archive the product.
UPDATE content_offers o SET state = 'withdrawn', revision = revision + 1, updated_at = now()
WHERE state = 'active' AND (
    (target_kind = 'content' AND NOT EXISTS (
        SELECT 1 FROM content_items c
        WHERE c.tenant_id=o.tenant_id AND c.id=o.target_id AND c.state<>'deleted'))
    OR (target_kind = 'version' AND NOT EXISTS (
        SELECT 1 FROM content_versions v JOIN content_items c
          ON c.tenant_id=v.tenant_id AND c.id=v.content_id
        WHERE v.tenant_id=o.tenant_id AND v.id=o.target_id
          AND v.state<>'deleted' AND c.state<>'deleted')));
