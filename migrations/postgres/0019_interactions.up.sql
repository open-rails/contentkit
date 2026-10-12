-- ContentKit #117 interactions: comments carry a language; tenant_id leads
-- the keys of comments and polls; facets carry dislikes; moderation rules and
-- recent searches are ContentKit data.

ALTER TABLE content_comments ADD COLUMN language text DEFAULT ''::text NOT NULL;
ALTER TABLE content_comments DROP CONSTRAINT content_comments_reply_to_id_fkey;
ALTER TABLE content_comments DROP CONSTRAINT content_comments_pkey;
ALTER TABLE content_comments ADD CONSTRAINT content_comments_pkey PRIMARY KEY (tenant_id, id);
ALTER TABLE content_comments ADD CONSTRAINT content_comments_reply_to_id_fkey
    FOREIGN KEY (tenant_id, reply_to_id) REFERENCES content_comments (tenant_id, id) ON DELETE CASCADE;
DROP INDEX content_comments_reply_idx;
CREATE INDEX content_comments_reply_idx ON content_comments (tenant_id, reply_to_id, created_at) WHERE (reply_to_id IS NOT NULL);
DROP INDEX content_comments_toplevel_idx;
CREATE INDEX content_comments_toplevel_idx ON content_comments (tenant_id, content_kind, content_id, content_version_id, language, created_at DESC)
    WHERE (reply_to_id IS NULL);

ALTER TABLE content_poll_options ADD COLUMN tenant_id text;
UPDATE content_poll_options o SET tenant_id = q.tenant_id FROM content_poll_questions q WHERE q.id = o.question_id;
ALTER TABLE content_poll_options ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE content_poll_votes ADD COLUMN tenant_id text;
UPDATE content_poll_votes v SET tenant_id = q.tenant_id FROM content_poll_questions q WHERE q.id = v.question_id;
ALTER TABLE content_poll_votes ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE content_poll_answers DROP CONSTRAINT content_poll_answers_question_id_fkey;
ALTER TABLE content_poll_options DROP CONSTRAINT content_poll_options_question_id_fkey;
ALTER TABLE content_poll_votes DROP CONSTRAINT content_poll_votes_option_id_fkey;
ALTER TABLE content_poll_votes DROP CONSTRAINT content_poll_votes_question_id_fkey;
ALTER TABLE content_poll_questions DROP CONSTRAINT content_poll_questions_pkey;
ALTER TABLE content_poll_questions ADD CONSTRAINT content_poll_questions_pkey PRIMARY KEY (tenant_id, id);
ALTER TABLE content_poll_options DROP CONSTRAINT content_poll_options_pkey;
ALTER TABLE content_poll_options ADD CONSTRAINT content_poll_options_pkey PRIMARY KEY (tenant_id, id);
ALTER TABLE content_poll_votes DROP CONSTRAINT content_poll_votes_pkey;
ALTER TABLE content_poll_votes ADD CONSTRAINT content_poll_votes_pkey PRIMARY KEY (tenant_id, id);
ALTER TABLE content_poll_answers DROP CONSTRAINT content_poll_answers_pkey;
ALTER TABLE content_poll_answers ADD CONSTRAINT content_poll_answers_pkey PRIMARY KEY (tenant_id, id);
ALTER TABLE content_poll_options ADD CONSTRAINT content_poll_options_question_id_fkey
    FOREIGN KEY (tenant_id, question_id) REFERENCES content_poll_questions (tenant_id, id) ON DELETE CASCADE;
ALTER TABLE content_poll_answers ADD CONSTRAINT content_poll_answers_question_id_fkey
    FOREIGN KEY (tenant_id, question_id) REFERENCES content_poll_questions (tenant_id, id) ON DELETE CASCADE;
ALTER TABLE content_poll_votes ADD CONSTRAINT content_poll_votes_question_id_fkey
    FOREIGN KEY (tenant_id, question_id) REFERENCES content_poll_questions (tenant_id, id) ON DELETE CASCADE;
ALTER TABLE content_poll_votes ADD CONSTRAINT content_poll_votes_option_id_fkey
    FOREIGN KEY (tenant_id, option_id) REFERENCES content_poll_options (tenant_id, id) ON DELETE CASCADE;
DROP INDEX content_poll_options_question_idx;
CREATE INDEX content_poll_options_question_idx ON content_poll_options (tenant_id, question_id, position);
DROP INDEX content_poll_votes_ip_uq;
CREATE UNIQUE INDEX content_poll_votes_ip_uq ON content_poll_votes (tenant_id, question_id, ip) WHERE ((user_id IS NULL) AND (ip IS NOT NULL));
DROP INDEX content_poll_votes_user_uq;
CREATE UNIQUE INDEX content_poll_votes_user_uq ON content_poll_votes (tenant_id, question_id, user_id) WHERE (user_id IS NOT NULL);
CREATE INDEX content_poll_votes_option_idx ON content_poll_votes (tenant_id, option_id);

ALTER TABLE content_facets ADD COLUMN dislikes integer DEFAULT 0 NOT NULL;

-- Staff-edited moderation rules: a banned word masks or refuses comment and
-- answer text; an IP ban (an address or a network) refuses them.
CREATE TABLE content_banned_words (
    tenant_id text NOT NULL,
    word text NOT NULL,
    action text DEFAULT 'mask'::text NOT NULL,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    PRIMARY KEY (tenant_id, word),
    CONSTRAINT content_banned_words_word_check CHECK (((btrim(word) <> ''::text) AND (word = lower(word)))),
    CONSTRAINT content_banned_words_action_check CHECK ((action = ANY (ARRAY['mask'::text, 'reject'::text])))
);

CREATE TABLE content_ip_bans (
    tenant_id text NOT NULL,
    network cidr NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    until timestamp with time zone,
    banned_by text NOT NULL,
    banned_at timestamp with time zone DEFAULT now() NOT NULL,
    PRIMARY KEY (tenant_id, network)
);

-- Recent searches: one row per subject (a user id, or s_<hash> of a
-- signed-out session) and normalized query; a clear hides what came before
-- it until the prune job deletes it.
CREATE TABLE content_searches (
    tenant_id text NOT NULL,
    subject text NOT NULL,
    normalized text NOT NULL,
    query text NOT NULL,
    results integer DEFAULT 0 NOT NULL,
    searches integer DEFAULT 1 NOT NULL,
    searched_at timestamp with time zone DEFAULT now() NOT NULL,
    PRIMARY KEY (tenant_id, subject, normalized)
);

CREATE INDEX content_searches_recent_idx ON content_searches (tenant_id, subject, searched_at DESC);
CREATE INDEX content_searches_age_idx ON content_searches (tenant_id, searched_at);

CREATE TABLE content_search_marks (
    tenant_id text NOT NULL,
    subject text NOT NULL,
    hidden_before timestamp with time zone NOT NULL,
    PRIMARY KEY (tenant_id, subject)
);
