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

-- A user-owned group's comment moderators ban commenters from the group and
-- the content it owns: scope 'group:<group id>'.
ALTER TABLE content_comment_bans DROP CONSTRAINT content_comment_bans_scope_ck;
ALTER TABLE content_comment_bans ADD CONSTRAINT content_comment_bans_scope_ck
    CHECK (scope = 'global' OR (scope LIKE 'owner:%' AND length(scope) > 6) OR (scope LIKE 'group:%' AND length(scope) > 6));

-- No version is NULL everywhere, never '': interactions, search and media
-- keyed by a target's version take NULL for the work itself, and keys treat
-- NULLs as equal.
-- The search worker deletes a key's invalid row with its dirty row.
DO $$
DECLARE fk text;
BEGIN
    SELECT conname INTO STRICT fk FROM pg_constraint WHERE conrelid = 'content_search_invalid'::regclass AND contype = 'f';
    EXECUTE format('ALTER TABLE content_search_invalid DROP CONSTRAINT %I', fk);
END;
$$;
ALTER TABLE content_search_invalid DROP CONSTRAINT content_search_invalid_pkey;
ALTER TABLE content_search_dirty DROP CONSTRAINT content_search_dirty_pkey;
ALTER TABLE content_search_documents DROP CONSTRAINT content_search_documents_pkey;
ALTER TABLE content_interaction_counts DROP CONSTRAINT content_interaction_counts_pkey;
ALTER TABLE content_favorites DROP CONSTRAINT content_favorites_pkey;
ALTER TABLE content_media_publications DROP CONSTRAINT content_media_publications_pkey;
DROP INDEX content_reactions_ip_uq;
DROP INDEX content_reactions_user_uq;

ALTER TABLE content_search_invalid ALTER COLUMN content_version_id DROP NOT NULL;
ALTER TABLE content_search_dirty ALTER COLUMN content_version_id DROP NOT NULL, ALTER COLUMN content_version_id DROP DEFAULT;
ALTER TABLE content_search_documents ALTER COLUMN content_version_id DROP NOT NULL, ALTER COLUMN content_version_id DROP DEFAULT;
ALTER TABLE content_comments ALTER COLUMN content_version_id DROP NOT NULL, ALTER COLUMN content_version_id DROP DEFAULT;
ALTER TABLE content_interaction_counts ALTER COLUMN content_version_id DROP NOT NULL, ALTER COLUMN content_version_id DROP DEFAULT;
ALTER TABLE content_favorites ALTER COLUMN content_version_id DROP NOT NULL, ALTER COLUMN content_version_id DROP DEFAULT;
ALTER TABLE content_reactions ALTER COLUMN content_version_id DROP NOT NULL, ALTER COLUMN content_version_id DROP DEFAULT;
ALTER TABLE content_media_publications ALTER COLUMN content_version_id DROP NOT NULL, ALTER COLUMN content_version_id DROP DEFAULT;
ALTER TABLE content_media_commits ALTER COLUMN content_version_id DROP NOT NULL, ALTER COLUMN content_version_id DROP DEFAULT;

UPDATE content_search_invalid SET content_version_id = NULL WHERE content_version_id = '';
UPDATE content_search_dirty SET content_version_id = NULL WHERE content_version_id = '';
UPDATE content_search_documents SET content_version_id = NULL WHERE content_version_id = '';
UPDATE content_comments SET content_version_id = NULL WHERE content_version_id = '';
UPDATE content_interaction_counts SET content_version_id = NULL WHERE content_version_id = '';
UPDATE content_favorites SET content_version_id = NULL WHERE content_version_id = '';
UPDATE content_reactions SET content_version_id = NULL WHERE content_version_id = '';
UPDATE content_media_publications SET content_version_id = NULL WHERE content_version_id = '';
UPDATE content_media_commits SET content_version_id = NULL WHERE content_version_id = '';

ALTER TABLE content_search_invalid ADD CONSTRAINT content_search_invalid_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, language),
    ADD CONSTRAINT content_search_invalid_version_check CHECK (content_version_id <> '');
ALTER TABLE content_search_dirty ADD CONSTRAINT content_search_dirty_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, language),
    ADD CONSTRAINT content_search_dirty_version_check CHECK (content_version_id <> '');
ALTER TABLE content_search_documents ADD CONSTRAINT content_search_documents_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, language),
    ADD CONSTRAINT content_search_documents_version_check CHECK (content_version_id <> '');
ALTER TABLE content_comments ADD CONSTRAINT content_comments_version_check CHECK (content_version_id <> '');
ALTER TABLE content_interaction_counts ADD CONSTRAINT content_interaction_counts_key UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id),
    ADD CONSTRAINT content_interaction_counts_version_check CHECK (content_version_id <> '');
ALTER TABLE content_favorites ADD CONSTRAINT content_favorites_key UNIQUE NULLS NOT DISTINCT (tenant_id, user_id, content_kind, content_id, content_version_id),
    ADD CONSTRAINT content_favorites_version_check CHECK (content_version_id <> '');
ALTER TABLE content_reactions ADD CONSTRAINT content_reactions_version_check CHECK (content_version_id <> '');
CREATE UNIQUE INDEX content_reactions_ip_uq ON content_reactions (tenant_id, content_kind, content_id, content_version_id, ip) NULLS NOT DISTINCT
    WHERE ((user_id IS NULL) AND (ip IS NOT NULL));
CREATE UNIQUE INDEX content_reactions_user_uq ON content_reactions (tenant_id, content_kind, content_id, content_version_id, user_id) NULLS NOT DISTINCT
    WHERE (user_id IS NOT NULL);
ALTER TABLE content_media_publications ADD CONSTRAINT content_media_publications_key
    UNIQUE NULLS NOT DISTINCT (tenant_id, content_kind, content_id, content_version_id, upload_name, upload_path, preset),
    ADD CONSTRAINT content_media_publications_version_check CHECK (content_version_id <> '');
ALTER TABLE content_media_commits ADD CONSTRAINT content_media_commits_version_check CHECK (content_version_id <> '');
