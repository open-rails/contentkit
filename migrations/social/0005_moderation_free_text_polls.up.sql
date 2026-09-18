-- parent: 4 sha256:cd835767274242b70d0fcb6ea70e92bfe33ced46ce6164f5995809df0f361285
-- Moderation state for comments and posts, and free-text polls.
--
-- `moderation` is the ContentModerator outcome of the item's latest write:
-- 'approved' publishes, 'held' awaits a reviewer (visible only to the author,
-- with the reason; never counted), 'rejected' is a reviewer's final verdict
-- (author-visible with the reason). moderation_verdict keeps the screening
-- provenance (model, prompt_version, confidence, or the moderator error that
-- forced the hold); moderated_by/moderated_at record the resolution.
ALTER TABLE social_comments
    ADD COLUMN moderation         text NOT NULL DEFAULT 'approved',
    ADD COLUMN moderation_reason  text,
    ADD COLUMN moderation_verdict jsonb,
    ADD COLUMN moderated_by       text,
    ADD COLUMN moderated_at       timestamptz,
    ADD CONSTRAINT social_comments_moderation_ck CHECK (moderation IN ('approved', 'held', 'rejected'));
CREATE INDEX social_comments_held_idx
    ON social_comments (tenant_id, created_at, id) WHERE moderation = 'held';

ALTER TABLE social_posts
    ADD COLUMN moderation         text NOT NULL DEFAULT 'approved',
    ADD COLUMN moderation_reason  text,
    ADD COLUMN moderation_verdict jsonb,
    ADD COLUMN moderated_by       text,
    ADD COLUMN moderated_at       timestamptz,
    ADD CONSTRAINT social_posts_moderation_ck CHECK (moderation IN ('approved', 'held', 'rejected'));
CREATE INDEX social_posts_held_idx
    ON social_posts (tenant_id, created_at, id) WHERE moderation = 'held';

-- Polls: a kind and an explicit close. A free_text poll takes one answer per
-- actor (editable until close) instead of options and votes; the host's
-- AnswerClassifier groups the answers. closes_at also closes voting.
ALTER TABLE social_poll_questions
    ADD COLUMN kind      text NOT NULL DEFAULT 'multiple_choice',
    ADD COLUMN closes_at timestamptz,
    ADD CONSTRAINT social_poll_questions_kind_ck CHECK (kind IN ('multiple_choice', 'free_text'));

CREATE TABLE social_poll_answers (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     text        NOT NULL,
    question_id   uuid        NOT NULL REFERENCES social_poll_questions (id) ON DELETE CASCADE,
    actor_id      text        NOT NULL,
    text          text        NOT NULL,
    group_id      text,                 -- the classifier's assignment; NULL until classified
    classified_at timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT social_poll_answers_actor_uq UNIQUE (tenant_id, question_id, actor_id)
);
-- Unclassified answers in write order (ReclassifyPending).
CREATE INDEX social_poll_answers_pending_idx
    ON social_poll_answers (tenant_id, updated_at) WHERE classified_at IS NULL;
