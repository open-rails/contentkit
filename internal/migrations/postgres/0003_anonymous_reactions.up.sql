-- A signed-out reaction is its browser's, keyed by the anonymous subject every
-- other signed-out row uses, never by the client's address: one reaction per
-- item and person. A reaction's person is exactly one of an account
-- (user_id), a signed-out browser (anonymous_subject), or one no longer known
-- (anonymous_actor_id: an erased account, an expired or folded browser, or an
-- imported signed-out reactor by a hash of its source key). Every row counts.
ALTER TABLE content_reactions DROP CONSTRAINT content_reactions_actor_ck;
DROP INDEX content_reactions_ip_uq;
DROP INDEX content_reactions_anonymous_idx;

-- A browser that reacted from several addresses keeps its latest reaction;
-- its others, and rows no browser claims, lose their person and keep counting.
UPDATE content_reactions r SET anonymous_subject = NULL, anonymous_actor_id = gen_random_uuid()
FROM (SELECT ctid AS id, row_number() OVER (PARTITION BY tenant_id, anonymous_subject, content_kind, content_id, content_version_id ORDER BY updated_at DESC) AS n
      FROM content_reactions WHERE user_id IS NULL AND anonymous_subject IS NOT NULL) d
WHERE r.ctid = d.id AND d.n > 1;
UPDATE content_reactions SET anonymous_actor_id = gen_random_uuid()
WHERE user_id IS NULL AND anonymous_subject IS NULL AND anonymous_actor_id IS NULL;

ALTER TABLE content_reactions DROP COLUMN ip;
ALTER TABLE content_reactions ADD CONSTRAINT content_reactions_actor_ck CHECK ((num_nonnulls(user_id, anonymous_subject, anonymous_actor_id) = 1));
CREATE UNIQUE INDEX content_reactions_anonymous_uq ON content_reactions USING btree (tenant_id, anonymous_subject, content_kind, content_id, content_version_id) NULLS NOT DISTINCT WHERE (anonymous_subject IS NOT NULL);
CREATE UNIQUE INDEX content_reactions_actor_uq ON content_reactions USING btree (tenant_id, content_kind, content_id, content_version_id, anonymous_actor_id) NULLS NOT DISTINCT WHERE (anonymous_actor_id IS NOT NULL);

-- A feedback prompt's signed-out person is named as on reactions.
ALTER TABLE content_reaction_feedback RENAME COLUMN anonymous_actor_id TO anonymous_subject;
