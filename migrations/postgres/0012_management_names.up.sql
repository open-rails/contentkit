-- Current operator text is separate from public approved/published documents.
-- Views remain tenant-keyed; every caller also applies write/creator scope in
-- SQL before retrieval limits. Existing 0.103 search writers remain valid.
CREATE VIEW content_management_content_names AS
SELECT c.tenant_id, c.kind AS content_kind, c.id AS content_id,
    NULL::text AS content_version_id, t.language, t.title, t.aliases
FROM content_items c JOIN content_text t ON t.tenant_id=c.tenant_id AND t.content_id=c.id
WHERE c.state <> 'deleted';

CREATE VIEW content_management_entity_names AS
SELECT g.tenant_id, g.kind AS content_kind, g.id AS content_id,
    NULL::text AS content_version_id, n.language, n.name AS title, '{}'::text[] AS aliases
FROM content_groups g JOIN content_group_names n ON n.tenant_id=g.tenant_id AND n.group_id=g.id
WHERE g.state='active';

CREATE INDEX content_text_management_prefix ON content_text
USING pgroonga (contentkit_search_terms(title, aliases, '{}'::text[], '') public.pgroonga_text_array_term_search_ops_v2);
CREATE INDEX content_text_management_substring ON content_text
USING pgroonga (contentkit_search_names(title, aliases) public.pgroonga_text_regexp_ops_v2);
CREATE INDEX content_group_names_management_prefix ON content_group_names
USING pgroonga (contentkit_search_terms(name, '{}'::text[], '{}'::text[], '') public.pgroonga_text_array_term_search_ops_v2);
CREATE INDEX content_group_names_management_substring ON content_group_names
USING pgroonga (contentkit_search_names(name, '{}'::text[]) public.pgroonga_text_regexp_ops_v2);
