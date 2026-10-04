-- parent: 8 sha256:1582d9a394e920cef1dbe3397a222263e8c74e754bf857d125a9246d8c523623

-- Comment bans hold current state only (no history). scope is 'global' (the
-- tenant) or 'owner:<owner id>' (content that owner owns). A NULL until holds
-- until lifted; an expired ban stays, shown as expired, until lifted or replaced.
CREATE TABLE content_comment_bans (
    tenant_id text NOT NULL,
    user_id text NOT NULL,
    scope text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    until timestamp with time zone,
    banned_by text NOT NULL,
    banned_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_comment_bans_pkey PRIMARY KEY (tenant_id, user_id, scope),
    CONSTRAINT content_comment_bans_scope_ck CHECK (scope = 'global' OR (scope LIKE 'owner:%' AND length(scope) > 6))
);

CREATE INDEX content_comment_bans_scope_idx ON content_comment_bans USING btree (tenant_id, scope, banned_at DESC, user_id);
