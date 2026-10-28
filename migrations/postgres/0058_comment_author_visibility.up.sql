ALTER TABLE content_comments
    ADD COLUMN author_hidden boolean NOT NULL DEFAULT false,
    DROP COLUMN published_body;

ALTER TABLE content_reactions
    ADD COLUMN author_hidden boolean NOT NULL DEFAULT false;

CREATE TABLE content_hidden_authors (
    tenant_id text NOT NULL,
    actor_id text NOT NULL,
    PRIMARY KEY (tenant_id, actor_id)
);
