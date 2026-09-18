-- parent: 2 sha256:1e716a58d3d2d664dea90ace6b023bef67f7a4b24cf86575ec2c09cff9e2f562
-- Add canonical inputs without changing existing applied baselines.
ALTER TABLE search_documents ADD COLUMN title text NOT NULL DEFAULT '';
ALTER TABLE search_documents ADD COLUMN aliases text[] NOT NULL DEFAULT '{}';
ALTER TABLE search_documents ADD COLUMN keywords text[] NOT NULL DEFAULT '{}';

CREATE FUNCTION searchkit_keyword_normalize(value text)
RETURNS text LANGUAGE sql IMMUTABLE PARALLEL SAFE
-- Strip the common Latin accent block only, then recompose. Japanese dakuten
-- (U+3099/U+309A) and Hangul composition must survive unchanged.
RETURN trim(regexp_replace(lower(normalize(regexp_replace(
    normalize(coalesce(value, ''), NFKD), U&'[\0300-\036f]', '', 'g'), NFC)), '\s+', ' ', 'g'));

-- Preserve legacy raw strings as a read adapter until the host reindexes with
-- structured fields. Do not guess title boundaries or discard existing data.
CREATE FUNCTION searchkit_keyword_terms(title text, aliases text[], keywords text[], legacy text)
RETURNS text[] LANGUAGE sql IMMUTABLE PARALLEL SAFE
RETURN (SELECT coalesce(array_agg(searchkit_keyword_normalize(term) ORDER BY ordinal), '{}')
    FROM unnest(ARRAY[coalesce(nullif(title, ''), legacy, '')] || aliases || keywords)
         WITH ORDINALITY AS input_terms(term, ordinal));

CREATE FUNCTION searchkit_keyword_text(title text, aliases text[], keywords text[], legacy text)
RETURNS text LANGUAGE sql IMMUTABLE PARALLEL SAFE
RETURN searchkit_keyword_normalize(concat_ws(' ', coalesce(nullif(title, ''), legacy, ''), array_to_string(aliases, ' '), array_to_string(keywords, ' ')));

CREATE INDEX search_documents_keyword_exact ON search_documents
USING gin (searchkit_keyword_terms(title, aliases, keywords, raw_document));
CREATE INDEX search_documents_keyword_fuzzy ON search_documents
USING gist (searchkit_keyword_text(title, aliases, keywords, raw_document) gist_trgm_ops(siglen=64));
CREATE INDEX search_documents_keyword_native ON search_documents
USING pgroonga (searchkit_keyword_terms(title, aliases, keywords, raw_document));
