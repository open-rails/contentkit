-- parent: 7 sha256:95b592392007650f84ea3e023a603e4c0f45e40a67ba35d38d5e1ab8c16d4286

-- Interaction rows are keyed by the canonical lower-case content id. Case
-- spellings of one id were separate keys, so one actor could react to one
-- comment many times. Merge them (each actor's latest row wins, re-exported
-- under a fresh revision), recount the merged keys and refuse other spellings.
CREATE TEMP TABLE contentkit_case_keys ON COMMIT DROP AS
SELECT DISTINCT tenant_id, content_kind, lower(content_id) AS content_id, content_version_id FROM (
    SELECT tenant_id, content_kind, content_id, content_version_id FROM content_reactions
    UNION ALL SELECT tenant_id, content_kind, content_id, content_version_id FROM content_favorites
    UNION ALL SELECT tenant_id, content_kind, content_id, content_version_id FROM content_interaction_counts
    UNION ALL SELECT tenant_id, content_kind, content_id, content_version_id FROM content_comments
) spelled WHERE content_id <> lower(content_id);

DELETE FROM content_reactions r USING (
    SELECT x.ctid AS row_id, row_number() OVER (
        PARTITION BY x.tenant_id, x.content_kind, lower(x.content_id), x.content_version_id, x.user_id, CASE WHEN x.user_id IS NULL THEN x.ip END
        ORDER BY x.revision DESC) AS n
    FROM content_reactions x JOIN contentkit_case_keys k ON k.tenant_id = x.tenant_id AND k.content_kind = x.content_kind
        AND k.content_id = lower(x.content_id) AND k.content_version_id = x.content_version_id
) d WHERE r.ctid = d.row_id AND d.n > 1;
UPDATE content_reactions SET content_id = lower(content_id), revision = nextval('content_preference_revision_seq'), updated_at = now()
WHERE content_id <> lower(content_id);

DELETE FROM content_favorites f USING (
    SELECT x.ctid AS row_id, row_number() OVER (
        PARTITION BY x.tenant_id, x.user_id, x.content_kind, lower(x.content_id), x.content_version_id
        ORDER BY x.revision DESC) AS n
    FROM content_favorites x JOIN contentkit_case_keys k ON k.tenant_id = x.tenant_id AND k.content_kind = x.content_kind
        AND k.content_id = lower(x.content_id) AND k.content_version_id = x.content_version_id
) d WHERE f.ctid = d.row_id AND d.n > 1;
UPDATE content_favorites SET content_id = lower(content_id), revision = nextval('content_preference_revision_seq'), updated_at = now()
WHERE content_id <> lower(content_id);

UPDATE content_comments SET content_id = lower(content_id) WHERE content_id <> lower(content_id);

CREATE TEMP TABLE contentkit_case_counts ON COMMIT DROP AS
SELECT k.*,
    (SELECT count(*) FROM content_reactions r WHERE r.tenant_id = k.tenant_id AND r.content_kind = k.content_kind
        AND r.content_id = k.content_id AND r.content_version_id = k.content_version_id AND r.value = 1)::int AS likes,
    (SELECT count(*) FROM content_reactions r WHERE r.tenant_id = k.tenant_id AND r.content_kind = k.content_kind
        AND r.content_id = k.content_id AND r.content_version_id = k.content_version_id AND r.value = -1)::int AS dislikes,
    (SELECT count(*) FROM content_favorites f WHERE f.tenant_id = k.tenant_id AND f.content_kind = k.content_kind
        AND f.content_id = k.content_id AND f.content_version_id = k.content_version_id AND f.value = 1)::int AS favorites,
    (SELECT coalesce(sum(c.comment_count), 0) FROM content_interaction_counts c WHERE c.tenant_id = k.tenant_id AND c.content_kind = k.content_kind
        AND lower(c.content_id) = k.content_id AND c.content_version_id = k.content_version_id)::int AS comment_count
FROM contentkit_case_keys k;

DELETE FROM content_interaction_counts c USING contentkit_case_keys k
WHERE c.tenant_id = k.tenant_id AND c.content_kind = k.content_kind AND lower(c.content_id) = k.content_id AND c.content_version_id = k.content_version_id;
INSERT INTO content_interaction_counts (tenant_id, content_kind, content_id, content_version_id, likes, dislikes, favorites, comment_count)
SELECT tenant_id, content_kind, content_id, content_version_id, likes, dislikes, favorites, comment_count FROM contentkit_case_counts;

UPDATE content_comments c SET likes = n.likes, dislikes = n.dislikes FROM contentkit_case_counts n
WHERE n.content_kind = 'comment' AND n.content_version_id = '' AND c.tenant_id = n.tenant_id AND c.id::text = n.content_id;

ALTER TABLE content_reactions ADD CONSTRAINT content_reactions_content_id_ck CHECK (content_id = lower(content_id));
ALTER TABLE content_favorites ADD CONSTRAINT content_favorites_content_id_ck CHECK (content_id = lower(content_id));
ALTER TABLE content_interaction_counts ADD CONSTRAINT content_interaction_counts_content_id_ck CHECK (content_id = lower(content_id));
ALTER TABLE content_comments ADD CONSTRAINT content_comments_content_id_ck CHECK (content_id = lower(content_id));
