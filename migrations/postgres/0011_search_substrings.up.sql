-- Search-only normalization. Entity identity, alias uniqueness and slugs keep
-- contentkit_keyword_normalize unchanged. Old functions/indexes remain usable
-- by 0.103 writers during a rolling upgrade.
CREATE FUNCTION contentkit_search_normalize(value text) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN btrim(regexp_replace(
        regexp_replace(contentkit_keyword_normalize(value), '\m(the|a|an)\M', '', 'g'),
        '\s+', ' ', 'g'));

CREATE FUNCTION contentkit_search_terms(title text, aliases text[], keywords text[], legacy text) RETURNS text[]
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN (SELECT coalesce(array_agg(contentkit_search_normalize(term) ORDER BY ordinal), '{}'::text[])
        FROM unnest(ARRAY[coalesce(nullif(title, ''), legacy, '')]
            || coalesce(aliases, '{}'::text[]) || coalesce(keywords, '{}'::text[]))
        WITH ORDINALITY AS input(term, ordinal));

CREATE FUNCTION contentkit_search_text(title text, aliases text[], keywords text[], legacy text) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN contentkit_search_normalize(concat_ws(' ', coalesce(nullif(title, ''), legacy, ''),
        array_to_string(aliases, ' '), array_to_string(keywords, ' ')));

CREATE FUNCTION contentkit_search_names(title text, aliases text[]) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN array_to_string(contentkit_search_terms(title, aliases, '{}'::text[], ''), E'\n');

CREATE INDEX content_search_documents_search_exact ON content_search_documents
USING gin (contentkit_search_terms(title, aliases, keywords, raw_document));
CREATE INDEX content_search_documents_search_fuzzy ON content_search_documents
USING gist (contentkit_search_text(title, aliases, keywords, raw_document) public.gist_trgm_ops (siglen='64'));
CREATE INDEX content_search_documents_search_native ON content_search_documents
USING pgroonga (contentkit_search_terms(title, aliases, keywords, raw_document));

-- The regexp operator class supports literal LIKE substrings in alphabetic
-- languages too. The default full-text tokenizer misses interior Latin text.
-- https://pgroonga.github.io/reference/operators/like.html
CREATE INDEX content_search_documents_search_substring ON content_search_documents
USING pgroonga (contentkit_search_text(title, aliases, keywords, raw_document) public.pgroonga_text_regexp_ops_v2);
CREATE INDEX content_search_documents_names_prefix ON content_search_documents
USING pgroonga (contentkit_search_terms(title, aliases, '{}'::text[], '') public.pgroonga_text_array_term_search_ops_v2);
CREATE INDEX content_search_documents_names_substring ON content_search_documents
USING pgroonga (contentkit_search_names(title, aliases) public.pgroonga_text_regexp_ops_v2);
