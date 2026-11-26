-- Per-person reading state, history and identity links, written by the
-- analytics job from ClickHouse's activity log. A subject is the account id
-- or the anonymous id the engine derives from the browser's.

-- Each gallery a person opened or hid. The job merges by maximum and latest,
-- so repeating it is harmless; hide and unhide write hidden at once.
-- best_outcome: 0 none, 1 bounce, 2 neutral, 3 good, 4 complete; position is
-- the last reported point, NULL until one is.
CREATE TABLE reading_state (
    app_id uuid NOT NULL,
    subject uuid NOT NULL,
    content_id uuid NOT NULL,
    first_day date,
    last_seen_at timestamp with time zone,
    best_outcome smallint NOT NULL DEFAULT 0,
    furthest integer NOT NULL DEFAULT 0,
    "position" integer,
    hidden boolean NOT NULL DEFAULT false,
    CONSTRAINT reading_state_pkey PRIMARY KEY (app_id, subject, content_id),
    CONSTRAINT reading_state_outcome_check CHECK (best_outcome BETWEEN 0 AND 4),
    CONSTRAINT reading_state_points_check CHECK (furthest >= 0 AND "position" >= 0),
    CONSTRAINT reading_state_opened_check CHECK ((first_day IS NULL) = (last_seen_at IS NULL)),
    CONSTRAINT reading_state_kept_check CHECK (hidden OR last_seen_at IS NOT NULL)
);

CREATE INDEX reading_state_recent_idx ON reading_state (app_id, subject, last_seen_at DESC) WHERE last_seen_at IS NOT NULL;
CREATE INDEX reading_state_content_idx ON reading_state (app_id, content_id);

-- One row per person and UTC day: the day's actions in time order, packed.
-- A version byte (2), then per action: a header byte (kind in bits 0-1:
-- 0 gallery, 1 search, 2 follow; bit 2 marks a session start), the UTC offset
-- in 15-minute steps (int8), the uvarint minute of the UTC day, then for a
-- gallery its 16-byte id, a byte (fraction read in tenths, bits 0-3; the
-- reaction, bits 4-6: 0 none, 1 like, 2 dislike, 3 favorite, 4 hidden), an
-- active-time byte (0 none, n for [2^(n-1), 2^n) seconds) and the surface of
-- the day's first open (readingstore.Surface: 0 outside, 1 home, 2 list,
-- 3 popular, 4 similar, 5 search, 6 entity, 7 recommended, 8 continue or
-- history, 9 favorites, 15 other); for a search a uvarint length and the
-- UTF-8 query; for a follow the 16-byte entity id.
CREATE TABLE reading_history (
    app_id uuid NOT NULL,
    subject uuid NOT NULL,
    day date NOT NULL,
    actions bytea NOT NULL,
    CONSTRAINT reading_history_pkey PRIMARY KEY (app_id, subject, day),
    CONSTRAINT reading_history_actions_check CHECK (octet_length(actions) > 1)
);

-- An anonymous id signed in as this account, sent once by the browser at
-- sign-in. The job folds the anonymous history into the account; later rows
-- from the anonymous id count as the account's.
CREATE TABLE anonymous_links (
    app_id uuid NOT NULL,
    anonymous_id uuid NOT NULL,
    account_id uuid NOT NULL,
    linked_at timestamp with time zone NOT NULL DEFAULT now(),
    folded_at timestamp with time zone,
    CONSTRAINT anonymous_links_pkey PRIMARY KEY (app_id, anonymous_id),
    CONSTRAINT anonymous_links_distinct_check CHECK (anonymous_id <> account_id)
);

CREATE INDEX anonymous_links_account_idx ON anonymous_links (app_id, account_id);
CREATE INDEX anonymous_links_unfolded_idx ON anonymous_links (app_id, linked_at) WHERE folded_at IS NULL;

-- A signed-out subject's latest activity: the analytics job records what it
-- folds from the log, and signed-out searches, reactions and hides record
-- themselves, so it holds without ClickHouse too. After
-- Config.AnonymousRetention without activity the subject's reading state,
-- history and recent searches are deleted and its reactions unlinked.
CREATE TABLE anonymous_activity (
    app_id uuid NOT NULL,
    subject uuid NOT NULL,
    last_active_at timestamp with time zone NOT NULL,
    CONSTRAINT anonymous_activity_pkey PRIMARY KEY (app_id, subject)
);
CREATE INDEX anonymous_activity_idle_idx ON anonymous_activity (app_id, last_active_at);

-- The value of an outcome: P(sequel) per segment and outcome, and q, its
-- position from -1 to +1 around the segment's base rate. Starting guesses
-- (fitted_at NULL) until the monthly refit writes measured values.
CREATE TABLE outcome_values (
    app_id uuid NOT NULL,
    segment text NOT NULL,
    outcome text NOT NULL,
    p_sequel double precision NOT NULL,
    q double precision NOT NULL,
    fitted_at timestamp with time zone,
    CONSTRAINT outcome_values_pkey PRIMARY KEY (app_id, segment, outcome),
    CONSTRAINT outcome_values_p_check CHECK (p_sequel >= 0 AND p_sequel <= 1),
    CONSTRAINT outcome_values_q_check CHECK (q >= -1 AND q <= 1)
);

-- The analytics job's progress: the activity ingested so far, when today's
-- rollups and the last closed day's were rebuilt, and when the ClickHouse
-- erasure last ran.
CREATE TABLE analytics_job (
    app_id uuid NOT NULL,
    ingested_through timestamp with time zone,
    rolled_up_at timestamp with time zone,
    closed_at timestamp with time zone,
    erased_at timestamp with time zone,
    erasures_through timestamp with time zone,
    CONSTRAINT analytics_job_pkey PRIMARY KEY (app_id)
);

-- A signed-out reaction keeps the browser's anonymous id beside its address,
-- so signing in re-keys it to the account.
ALTER TABLE content_reactions ADD COLUMN anonymous_subject uuid;
CREATE INDEX content_reactions_anonymous_idx ON content_reactions (tenant_id, anonymous_subject) WHERE anonymous_subject IS NOT NULL;
