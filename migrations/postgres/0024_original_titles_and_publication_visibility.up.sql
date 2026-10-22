-- Original/source titles are independent of localized edition display titles.
-- Existing localized text is retained unchanged; application-specific title
-- adoption runs explicitly after this schema migration.
ALTER TABLE content_items
    ADD COLUMN original_title text,
    ADD COLUMN original_language text,
    ADD CONSTRAINT content_items_original_title_length CHECK (char_length(original_title) <= 512),
    ADD CONSTRAINT content_items_original_language_length CHECK (char_length(original_language) <= 128);

-- Publication, access and domain filters now determine visibility. Every
-- formerly unlisted/members-only listing becomes an ordinary published item;
-- its publication state and access rule remain authoritative.
-- Preserve earlier availability facts; append a boundary at adoption so the
-- retired classification only stops applying from this migration onward.
INSERT INTO content_access_history (tenant_id,content_id,version_id,occurred_at,snapshot)
SELECT tenant_id,content_id,version_id,statement_timestamp(),snapshot - 'listing'
FROM (SELECT DISTINCT ON (tenant_id,content_id,version_id)
      tenant_id,content_id,version_id,snapshot
      FROM content_access_history
      ORDER BY tenant_id,content_id,version_id,occurred_at DESC,id DESC) latest;
ALTER TABLE content_facets DROP COLUMN listing;
ALTER TABLE content_items DROP COLUMN listing;

-- Operator lookup searches source and edition titles, including drafts. It
-- still applies caller eligibility before returning any result.
CREATE OR REPLACE VIEW content_management_content_names AS
SELECT c.tenant_id,c.kind AS content_kind,c.id AS content_id,NULL::text AS content_version_id,t.language,t.title,t.aliases
FROM content_items c JOIN content_text t ON t.tenant_id=c.tenant_id AND t.content_id=c.id
WHERE c.state<>'deleted'
UNION ALL
SELECT c.tenant_id,c.kind,c.id,NULL::text,t.language,t.title,'{}'::text[]
FROM content_items c JOIN content_versions v ON v.tenant_id=c.tenant_id AND v.content_id=c.id
JOIN content_version_text t ON t.tenant_id=v.tenant_id AND t.version_id=v.id
WHERE c.state<>'deleted' AND v.state<>'deleted' AND t.title<>''
UNION ALL
SELECT DISTINCT c.tenant_id,c.kind,c.id,NULL::text,l.language,c.original_title,'{}'::text[]
FROM content_items c JOIN content_versions v ON v.tenant_id=c.tenant_id AND v.content_id=c.id
CROSS JOIN LATERAL unnest(v.languages) l(language)
WHERE c.state<>'deleted' AND v.state<>'deleted' AND coalesce(c.original_title,'')<>'';

CREATE INDEX content_version_text_management_prefix ON content_version_text
USING pgroonga (contentkit_search_terms(title, '{}'::text[], '{}'::text[], '') public.pgroonga_text_array_term_search_ops_v2);
CREATE INDEX content_version_text_management_substring ON content_version_text
USING pgroonga (contentkit_search_names(title, '{}'::text[]) public.pgroonga_text_regexp_ops_v2);
CREATE INDEX content_original_title_management_prefix ON content_items
USING pgroonga (contentkit_search_terms(original_title, '{}'::text[], '{}'::text[], '') public.pgroonga_text_array_term_search_ops_v2);
CREATE INDEX content_original_title_management_substring ON content_items
USING pgroonga (contentkit_search_names(original_title, '{}'::text[]) public.pgroonga_text_regexp_ops_v2);
