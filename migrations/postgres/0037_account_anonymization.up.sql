-- Recovery-period visibility is reversible. A purged account's temporary
-- anonymous signal key is removed after all explicitly invoked cleanup finishes.
CREATE TABLE content_account_lifecycle (
    tenant_id text NOT NULL,
    actor_id text NOT NULL,
    state text NOT NULL CHECK (state IN ('deleted', 'purging', 'purged')),
    anonymous_key uuid,
    PRIMARY KEY (tenant_id, actor_id),
    CHECK ((state = 'purging') = (anonymous_key IS NOT NULL))
);

-- Anonymous contributions remain real rows, with no account or IP surrogate.
ALTER TABLE content_reactions ADD COLUMN anonymous_actor_id uuid;
ALTER TABLE content_reactions DROP CONSTRAINT content_reactions_actor_ck;
ALTER TABLE content_reactions ADD CONSTRAINT content_reactions_actor_ck CHECK
    ((anonymous_actor_id IS NOT NULL AND user_id IS NULL AND ip IS NULL) OR (anonymous_actor_id IS NULL AND (user_id IS NOT NULL OR ip IS NOT NULL)));

ALTER TABLE content_favorites ADD COLUMN anonymous_actor_id uuid;
ALTER TABLE content_favorites ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE content_favorites DROP CONSTRAINT content_favorites_key;
CREATE UNIQUE INDEX content_favorites_user_key ON content_favorites
    (tenant_id, user_id, content_kind, content_id, content_version_id) NULLS NOT DISTINCT
    WHERE user_id IS NOT NULL;
ALTER TABLE content_favorites ADD CONSTRAINT content_favorites_actor_ck CHECK
    ((anonymous_actor_id IS NOT NULL AND user_id IS NULL) OR (anonymous_actor_id IS NULL AND user_id IS NOT NULL));

ALTER TABLE content_poll_votes ADD COLUMN anonymous_actor_id uuid;
ALTER TABLE content_poll_votes DROP CONSTRAINT content_poll_votes_actor_ck;
ALTER TABLE content_poll_votes ADD CONSTRAINT content_poll_votes_actor_ck CHECK
    ((anonymous_actor_id IS NOT NULL AND user_id IS NULL AND ip IS NULL) OR (anonymous_actor_id IS NULL AND (user_id IS NOT NULL OR ip IS NOT NULL)));
ALTER TABLE content_poll_answers ALTER COLUMN actor_id DROP NOT NULL;
