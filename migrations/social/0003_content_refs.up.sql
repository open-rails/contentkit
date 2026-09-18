-- parent: 2 sha256:b431d777d4b77ec4eddaef06ca11553aeff2632dbe043dc1b390ebb7a62d5a46
-- ContentKit content references. Every social row is keyed by
-- (tenant_id, content_kind, content_id, content_version_id): the ContentRef
-- of the host-owned work ('' version = the work itself). Rows that predate
-- this migration keep their stored ids under tenant_id = '' and are assigned
-- the host's tenant once by content.AssignTenant (docs/migration.md); no data
-- moves. Comment threading uses reply_to_id. Table names stay social_*.

-- Reactions
ALTER TABLE social_reactions RENAME COLUMN entity_type TO content_kind;
ALTER TABLE social_reactions RENAME COLUMN entity_id TO content_id;
ALTER TABLE social_reactions ADD COLUMN tenant_id text NOT NULL DEFAULT '';
ALTER TABLE social_reactions ALTER COLUMN tenant_id DROP DEFAULT;
ALTER TABLE social_reactions ADD COLUMN content_version_id text NOT NULL DEFAULT '';
DROP INDEX social_reactions_user_uq;
DROP INDEX social_reactions_ip_uq;
DROP INDEX social_reactions_entity_idx;
CREATE UNIQUE INDEX social_reactions_user_uq
    ON social_reactions (tenant_id, content_kind, content_id, content_version_id, user_id)
    WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX social_reactions_ip_uq
    ON social_reactions (tenant_id, content_kind, content_id, content_version_id, ip)
    WHERE user_id IS NULL AND ip IS NOT NULL;
CREATE INDEX social_reactions_content_idx
    ON social_reactions (tenant_id, content_kind, content_id, content_version_id);

-- Comments
ALTER TABLE social_comments RENAME COLUMN entity_type TO content_kind;
ALTER TABLE social_comments RENAME COLUMN entity_id TO content_id;
ALTER TABLE social_comments RENAME COLUMN parent_id TO reply_to_id;
ALTER TABLE social_comments RENAME CONSTRAINT social_comments_parent_id_fkey TO social_comments_reply_to_id_fkey;
ALTER TABLE social_comments ADD COLUMN tenant_id text NOT NULL DEFAULT '';
ALTER TABLE social_comments ALTER COLUMN tenant_id DROP DEFAULT;
ALTER TABLE social_comments ADD COLUMN content_version_id text NOT NULL DEFAULT '';
DROP INDEX social_comments_toplevel_idx;
DROP INDEX social_comments_parent_idx;
DROP INDEX social_comments_latest_idx;
CREATE INDEX social_comments_toplevel_idx
    ON social_comments (tenant_id, content_kind, content_id, content_version_id, created_at DESC)
    WHERE reply_to_id IS NULL;
CREATE INDEX social_comments_reply_idx
    ON social_comments (reply_to_id, created_at) WHERE reply_to_id IS NOT NULL;
CREATE INDEX social_comments_latest_idx
    ON social_comments (tenant_id, created_at DESC) WHERE deleted_at IS NULL;

-- Polls: the question carries the tenant; options and votes hang off it.
ALTER TABLE social_poll_questions ADD COLUMN tenant_id text NOT NULL DEFAULT '';
ALTER TABLE social_poll_questions ALTER COLUMN tenant_id DROP DEFAULT;
DROP INDEX social_poll_questions_live_idx;
CREATE INDEX social_poll_questions_live_idx
    ON social_poll_questions (tenant_id, language, live_at DESC) WHERE deleted_at IS NULL AND is_active;

-- Posts
ALTER TABLE social_posts ADD COLUMN tenant_id text NOT NULL DEFAULT '';
ALTER TABLE social_posts ALTER COLUMN tenant_id DROP DEFAULT;
DROP INDEX social_posts_published_idx;
DROP INDEX social_posts_slug_uq;
CREATE INDEX social_posts_published_idx
    ON social_posts (tenant_id, language, live_at DESC)
    WHERE deleted_at IS NULL AND is_draft = false;
CREATE UNIQUE INDEX social_posts_slug_uq
    ON social_posts (tenant_id, slug) WHERE slug IS NOT NULL AND deleted_at IS NULL;

-- Counts rollup
ALTER TABLE social_entity_counts RENAME COLUMN entity_type TO content_kind;
ALTER TABLE social_entity_counts RENAME COLUMN entity_id TO content_id;
ALTER TABLE social_entity_counts ADD COLUMN tenant_id text NOT NULL DEFAULT '';
ALTER TABLE social_entity_counts ALTER COLUMN tenant_id DROP DEFAULT;
ALTER TABLE social_entity_counts ADD COLUMN content_version_id text NOT NULL DEFAULT '';
ALTER TABLE social_entity_counts DROP CONSTRAINT social_entity_counts_pkey;
ALTER TABLE social_entity_counts ADD PRIMARY KEY (tenant_id, content_kind, content_id, content_version_id);
DROP INDEX social_entity_counts_likes_idx;
DROP INDEX social_entity_counts_favorites_idx;
DROP INDEX social_entity_counts_comments_idx;
DROP INDEX social_entity_counts_best_idx;
CREATE INDEX social_entity_counts_likes_idx     ON social_entity_counts (tenant_id, content_kind, likes DESC)         WHERE likes > 0;
CREATE INDEX social_entity_counts_favorites_idx ON social_entity_counts (tenant_id, content_kind, favorites DESC)     WHERE favorites > 0;
CREATE INDEX social_entity_counts_comments_idx  ON social_entity_counts (tenant_id, content_kind, comment_count DESC) WHERE comment_count > 0;
CREATE INDEX social_entity_counts_best_idx ON social_entity_counts (
    tenant_id, content_kind,
    (CASE WHEN likes + dislikes = 0 THEN 0::float8 ELSE
        ((likes + 1.9208) / (likes + dislikes)
         - 1.96 * sqrt((likes::float8 * dislikes) / (likes + dislikes) + 0.9604) / (likes + dislikes))
        / (1 + 3.8416 / (likes + dislikes))
     END) DESC
) WHERE likes + dislikes > 0;

-- Favorites
ALTER TABLE social_favorites RENAME COLUMN entity_type TO content_kind;
ALTER TABLE social_favorites RENAME COLUMN entity_id TO content_id;
ALTER TABLE social_favorites ADD COLUMN tenant_id text NOT NULL DEFAULT '';
ALTER TABLE social_favorites ALTER COLUMN tenant_id DROP DEFAULT;
ALTER TABLE social_favorites ADD COLUMN content_version_id text NOT NULL DEFAULT '';
ALTER TABLE social_favorites DROP CONSTRAINT social_favorites_pkey;
ALTER TABLE social_favorites ADD PRIMARY KEY (tenant_id, user_id, content_kind, content_id, content_version_id);
DROP INDEX social_favorites_entity_idx;
DROP INDEX social_favorites_user_created_idx;
CREATE INDEX social_favorites_content_idx
    ON social_favorites (tenant_id, content_kind, content_id, content_version_id);
CREATE INDEX social_favorites_user_created_idx
    ON social_favorites (tenant_id, user_id, created_at DESC);
