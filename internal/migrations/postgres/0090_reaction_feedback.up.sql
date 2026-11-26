-- A feedback prompt offered after a like, favorite or dislike: one row per
-- person and content item, completed when answered. The person is the account
-- (user_id) or a signed-out browser (anonymous_actor_id, its anonymous
-- subject). Account erasure and anonymous expiry delete the rows; text never
-- leaves this table. It is re-runnable, as the favorites conversion test
-- replays every migration after 0071.
CREATE TABLE IF NOT EXISTS content_reaction_feedback (
    app_id uuid NOT NULL,
    user_id uuid,
    anonymous_actor_id uuid,
    content_id uuid NOT NULL,
    reaction text NOT NULL,
    offered_at timestamp with time zone NOT NULL,
    answered_at timestamp with time zone,
    reason text,
    -- The tag, artist, series or character the answer names.
    entity_id text,
    -- The tag a not_into answer suggested hiding, and whether it was hidden
    -- (NULL until decided).
    hide_entity_id text,
    hide_taken boolean,
    text text,
    CONSTRAINT content_reaction_feedback_subject_ck CHECK ((user_id IS NULL) <> (anonymous_actor_id IS NULL)),
    CONSTRAINT content_reaction_feedback_reaction_ck CHECK (reaction IN ('like', 'favorite', 'dislike')),
    CONSTRAINT content_reaction_feedback_reason_ck CHECK (reason IS NULL
        OR reaction = 'dislike' AND reason IN ('bad_art', 'bad_story', 'not_into', 'misleading', 'other')
        OR reaction <> 'dislike' AND reason IN ('artwork', 'artist', 'story', 'series', 'tag', 'other')),
    CONSTRAINT content_reaction_feedback_answered_ck CHECK ((answered_at IS NULL) = (reason IS NULL)),
    CONSTRAINT content_reaction_feedback_entity_ck CHECK (CASE WHEN reason IN ('tag', 'not_into') THEN entity_id IS NOT NULL
        WHEN reason IN ('artist', 'series') THEN true ELSE entity_id IS NULL END),
    CONSTRAINT content_reaction_feedback_text_ck CHECK (text IS NULL OR reason = 'other' AND char_length(text) BETWEEN 1 AND 280),
    CONSTRAINT content_reaction_feedback_hide_ck CHECK (hide_taken IS NULL OR hide_entity_id IS NOT NULL)
);

CREATE UNIQUE INDEX IF NOT EXISTS content_reaction_feedback_user_key ON content_reaction_feedback (app_id, user_id, content_id) WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS content_reaction_feedback_anonymous_key ON content_reaction_feedback (app_id, anonymous_actor_id, content_id) WHERE anonymous_actor_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS content_reaction_feedback_content_idx ON content_reaction_feedback (app_id, content_id);
