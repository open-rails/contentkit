-- When content first went live, never cleared: a draft that never did is one
-- its kind's draft_ttl expires.
ALTER TABLE content_items ADD COLUMN first_published_at timestamp with time zone;
UPDATE content_items SET first_published_at = published_at WHERE published_at IS NOT NULL;

CREATE FUNCTION contentkit_first_published() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    NEW.first_published_at := coalesce(NEW.first_published_at, NEW.published_at);
    RETURN NEW;
END;
$$;

CREATE TRIGGER content_items_first_published BEFORE INSERT OR UPDATE OF published_at ON content_items
    FOR EACH ROW EXECUTE FUNCTION contentkit_first_published();

CREATE INDEX content_items_drafts_idx ON content_items (tenant_id, kind, created_at) WHERE ((state = 'draft'::text) AND (first_published_at IS NULL));

-- The user-owned group whose deletion deleted the content: restoring the
-- group restores it.
ALTER TABLE content_items ADD COLUMN deleted_with_group text;
CREATE INDEX content_items_deleted_with_group_idx ON content_items (tenant_id, deleted_with_group) WHERE (deleted_with_group IS NOT NULL);

-- A deleted group's media folder is purged after the retention unless it is
-- restored first; both follow its state.
ALTER TABLE content_groups ADD COLUMN deleted_at timestamp with time zone, ADD COLUMN media_purged_at timestamp with time zone;
UPDATE content_groups SET deleted_at = updated_at WHERE state = 'deleted';

CREATE FUNCTION contentkit_group_deleted() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.state <> 'deleted' THEN
        NEW.deleted_at := NULL;
        NEW.media_purged_at := NULL;
    ELSIF TG_OP = 'INSERT' OR OLD.state <> 'deleted' THEN
        NEW.deleted_at := coalesce(NEW.deleted_at, now());
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER content_groups_deleted BEFORE INSERT OR UPDATE OF state ON content_groups
    FOR EACH ROW EXECUTE FUNCTION contentkit_group_deleted();

CREATE INDEX content_groups_purge_idx ON content_groups (tenant_id, deleted_at) WHERE ((state = 'deleted'::text) AND (media_purged_at IS NULL));

-- A version's free_after has passed and what follows ran (catalog
-- ReleaseFree): facets, documents and media exposure re-derived. A changed
-- free_after runs again.
ALTER TABLE content_facets ADD COLUMN free_applied boolean DEFAULT false NOT NULL;
UPDATE content_facets SET free_applied = true WHERE access_free_after <= now();
CREATE INDEX content_facets_free_idx ON content_facets (tenant_id, access_free_after) WHERE ((NOT free_applied) AND (access_free_after IS NOT NULL));
