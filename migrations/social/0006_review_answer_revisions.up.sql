-- parent: 5 sha256:442a9463a2d25ece2101e58815b1b86be84af2893ccd747d119d4739263daf32
-- Bind reviewer decisions and classifier delivery to immutable source revisions.
ALTER TABLE social_comments ADD COLUMN moderation_revision bigint NOT NULL DEFAULT 1 CHECK (moderation_revision > 0);
ALTER TABLE social_posts ADD COLUMN moderation_revision bigint NOT NULL DEFAULT 1 CHECK (moderation_revision > 0);
ALTER TABLE social_poll_answers ADD COLUMN revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0);
ALTER TABLE social_poll_answers ADD COLUMN group_label text;
