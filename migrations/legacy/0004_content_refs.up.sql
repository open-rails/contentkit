-- parent: 3 sha256:ef68ec7f801c39594fc54901bf8fe7c762781acf97560bc9f114869179277550
-- ContentKit hard cut (contentkit #1): the keyword profile is keyed by
-- tenant-scoped content references (tenant, content kind, content id,
-- content version, language); a language is document metadata, never
-- identity. Rows that predate this migration keep tenant_id '' and are
-- unreachable by any tenant; the host's first backfill replaces them and
-- docs/migration.md removes them afterwards. FTS and trigram-typeahead
-- projections (document, tsv) leave with their readers.
ALTER TABLE search_documents RENAME TO content_search_documents;
ALTER TABLE search_dirty RENAME TO content_search_dirty;
ALTER TABLE search_documents_backfill_state RENAME TO content_search_backfill;

ALTER TABLE content_search_documents RENAME COLUMN entity_type TO content_kind;
ALTER TABLE content_search_documents RENAME COLUMN entity_id TO content_id;
ALTER TABLE content_search_documents ADD COLUMN tenant_id text NOT NULL DEFAULT '';
ALTER TABLE content_search_documents ALTER COLUMN tenant_id DROP DEFAULT;
ALTER TABLE content_search_documents ADD COLUMN content_version_id text NOT NULL DEFAULT '';
ALTER TABLE content_search_documents DROP CONSTRAINT search_documents_pkey;
ALTER TABLE content_search_documents ADD PRIMARY KEY (tenant_id, content_kind, content_id, content_version_id, language);
ALTER TABLE content_search_documents DROP COLUMN document;
ALTER TABLE content_search_documents DROP COLUMN tsv;
DROP INDEX idx_search_documents_entity_language;
DROP INDEX idx_search_documents_raw_document_pgroonga_cjk;
CREATE INDEX content_search_documents_kind_language ON content_search_documents (tenant_id, content_kind, language);
ALTER INDEX search_documents_keyword_exact RENAME TO content_search_documents_keyword_exact;
ALTER INDEX search_documents_keyword_fuzzy RENAME TO content_search_documents_keyword_fuzzy;
ALTER INDEX search_documents_keyword_native RENAME TO content_search_documents_keyword_native;

ALTER TABLE content_search_dirty RENAME COLUMN entity_type TO content_kind;
ALTER TABLE content_search_dirty RENAME COLUMN entity_id TO content_id;
ALTER TABLE content_search_dirty ADD COLUMN tenant_id text NOT NULL DEFAULT '';
ALTER TABLE content_search_dirty ALTER COLUMN tenant_id DROP DEFAULT;
ALTER TABLE content_search_dirty ADD COLUMN content_version_id text NOT NULL DEFAULT '';
ALTER TABLE content_search_dirty DROP CONSTRAINT search_dirty_pkey;
ALTER TABLE content_search_dirty ADD PRIMARY KEY (tenant_id, content_kind, content_id, content_version_id, language);
DROP INDEX idx_search_dirty_updated_at;
CREATE INDEX content_search_dirty_updated_at ON content_search_dirty (tenant_id, updated_at);
ALTER TRIGGER search_dirty_revision ON content_search_dirty RENAME TO content_search_dirty_revision;
ALTER FUNCTION searchkit_bump_dirty_revision() RENAME TO contentkit_bump_dirty_revision;
ALTER SEQUENCE search_dirty_revision_seq RENAME TO content_search_dirty_revision_seq;

ALTER TABLE content_search_backfill RENAME COLUMN entity_type TO content_kind;
ALTER TABLE content_search_backfill ADD COLUMN tenant_id text NOT NULL DEFAULT '';
ALTER TABLE content_search_backfill ALTER COLUMN tenant_id DROP DEFAULT;
ALTER TABLE content_search_backfill DROP CONSTRAINT search_documents_backfill_state_pkey;
ALTER TABLE content_search_backfill ADD PRIMARY KEY (tenant_id, content_kind, language);
ALTER INDEX idx_search_documents_backfill_state_state RENAME TO content_search_backfill_state;

ALTER FUNCTION searchkit_keyword_normalize(text) RENAME TO contentkit_keyword_normalize;
ALTER FUNCTION searchkit_keyword_terms(text, text[], text[], text) RENAME TO contentkit_keyword_terms;
ALTER FUNCTION searchkit_keyword_text(text, text[], text[], text) RENAME TO contentkit_keyword_text;
DROP FUNCTION searchkit_regconfig_for_language(text);
