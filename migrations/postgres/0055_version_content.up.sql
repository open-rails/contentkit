-- Authored content belongs to an edition. Parent items retain only shared
-- identity, authorization defaults and descriptive relationships.
ALTER TABLE content_versions
    ADD COLUMN original_title text,
    ADD COLUMN original_language text,
    ADD CONSTRAINT content_versions_original_title_length CHECK (char_length(original_title) <= 512),
    ADD CONSTRAINT content_versions_original_language_length CHECK (char_length(original_language) <= 128);
UPDATE content_versions v SET original_title=c.original_title,original_language=c.original_language
FROM content_items c WHERE c.tenant_id=v.tenant_id AND c.id=v.content_id;

ALTER TABLE content_version_text
    ADD COLUMN slug text NOT NULL DEFAULT '',
    ADD COLUMN body text NOT NULL DEFAULT '',
    ADD COLUMN sort_key text NOT NULL DEFAULT '',
    ADD COLUMN aliases text[] NOT NULL DEFAULT '{}',
    ADD COLUMN updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    ADD COLUMN body_excerpt text GENERATED ALWAYS AS (contentkit_body_excerpt(body)) STORED;

-- Preserve all existing parent translations on each existing edition. This is
-- one-time database adoption, not automatic copying on future version creation.
-- Existing edition titles/summaries win; uploaded source files are untouched.
INSERT INTO content_version_text AS existing
    (tenant_id,version_id,language,title,description,slug,body,sort_key,aliases,updated_at)
SELECT v.tenant_id,v.id,t.language,t.title,t.description,t.slug,t.body,t.sort_key,t.aliases,t.updated_at
FROM content_versions v JOIN content_text t ON t.tenant_id=v.tenant_id AND t.content_id=v.content_id
ON CONFLICT (tenant_id,version_id,language) DO UPDATE SET
    title=coalesce(nullif(existing.title,''),EXCLUDED.title),
    description=coalesce(nullif(existing.description,''),EXCLUDED.description),
    slug=EXCLUDED.slug,body=EXCLUDED.body,sort_key=EXCLUDED.sort_key,
    aliases=EXCLUDED.aliases,updated_at=greatest(existing.updated_at,EXCLUDED.updated_at);

CREATE OR REPLACE VIEW content_management_content_names AS
SELECT c.tenant_id,c.kind AS content_kind,c.id AS content_id,v.id AS content_version_id,t.language,t.title,t.aliases
FROM content_items c JOIN content_versions v ON v.tenant_id=c.tenant_id AND v.content_id=c.id
JOIN content_version_text t ON t.tenant_id=v.tenant_id AND t.version_id=v.id
WHERE c.state<>'deleted' AND v.state<>'deleted'
UNION ALL
SELECT c.tenant_id,c.kind,c.id,v.id,l.language,v.original_title,'{}'::text[]
FROM content_items c JOIN content_versions v ON v.tenant_id=c.tenant_id AND v.content_id=c.id
CROSS JOIN LATERAL unnest(v.languages) l(language)
WHERE c.state<>'deleted' AND v.state<>'deleted' AND coalesce(v.original_title,'')<>'';

DROP INDEX content_version_text_management_prefix;
DROP INDEX content_version_text_management_substring;
CREATE INDEX content_version_text_management_prefix ON content_version_text
USING pgroonga (contentkit_search_terms(title, aliases, '{}'::text[], '') public.pgroonga_text_array_term_search_ops_v2);
CREATE INDEX content_version_text_management_substring ON content_version_text
USING pgroonga (contentkit_search_names(title, aliases) public.pgroonga_text_regexp_ops_v2);
CREATE INDEX content_version_original_title_management_prefix ON content_versions
USING pgroonga (contentkit_search_terms(original_title, '{}'::text[], '{}'::text[], '') public.pgroonga_text_array_term_search_ops_v2);
CREATE INDEX content_version_original_title_management_substring ON content_versions
USING pgroonga (contentkit_search_names(original_title, '{}'::text[]) public.pgroonga_text_regexp_ops_v2);

DROP TABLE content_text;
ALTER TABLE content_items DROP COLUMN original_title,DROP COLUMN original_language;
