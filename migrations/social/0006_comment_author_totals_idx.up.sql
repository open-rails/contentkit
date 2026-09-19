-- parent: 5 sha256:1e9d258387f25d221f284d33c21e29f1794515dc426dc1f5404747c8fe494bcd
-- Per-author totals over published comments (CommentReactionsByAuthor).
-- INCLUDE keeps the aggregate index-only; the partial predicate matches the
-- read exactly, so held, rejected and tombstoned rows stay out of the index.
CREATE INDEX IF NOT EXISTS social_comments_author_published_idx
    ON social_comments (tenant_id, user_id) INCLUDE (likes, dislikes)
    WHERE deleted_at IS NULL AND moderation = 'approved' AND user_id IS NOT NULL;
