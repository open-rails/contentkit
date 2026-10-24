-- Keyword retrieval reads normalized terms stored once per document instead of
-- normalizing every candidate on every request.
DROP INDEX IF EXISTS content_search_documents_keyword_exact;
DROP INDEX IF EXISTS content_search_documents_keyword_fuzzy;
DROP INDEX IF EXISTS content_search_documents_keyword_native;
DROP INDEX IF EXISTS content_search_documents_name_prefix;
DROP INDEX content_search_documents_search_exact;
DROP INDEX content_search_documents_search_fuzzy;
DROP INDEX content_search_documents_search_native;
DROP INDEX content_search_documents_search_substring;
DROP FUNCTION IF EXISTS contentkit_keyword_terms(text, text[], text[], text);
DROP FUNCTION IF EXISTS contentkit_keyword_text(text, text[], text[], text);

ALTER TABLE content_search_documents
    DROP COLUMN raw_document,
    ADD COLUMN terms text[] GENERATED ALWAYS AS (contentkit_search_terms(title, aliases, keywords, '')) STORED,
    ADD COLUMN body text GENERATED ALWAYS AS (contentkit_search_text(title, aliases, keywords, '')) STORED;

CREATE INDEX content_search_documents_terms ON content_search_documents USING gin (terms);
CREATE INDEX content_search_documents_terms_native ON content_search_documents USING pgroonga (terms);
CREATE INDEX content_search_documents_body_substring ON content_search_documents
USING pgroonga (body public.pgroonga_text_regexp_ops_v2);
CREATE INDEX content_search_documents_body_fuzzy ON content_search_documents
USING gist (body public.gist_trgm_ops (siglen='64'));
