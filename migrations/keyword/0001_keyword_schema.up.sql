-- parent: root
-- Fresh keyword-only profile. Apply under its own migration group/ledger.
-- Existing combined installations must keep migrations.Postgres and its ledger;
-- switching profiles is not an in-place conversion or semantic provisioning.
-- Three tables, 22 columns. Typed title/aliases/keywords remain separate work.

-- pg_trgm provides trigram similarity operators + GIN index support.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- pgroonga backs native-script (CJK/Korean) full-text matching. Requires
-- superuser or elevated privileges; if your environment can't run CREATE
-- EXTENSION from app migrations, install/enable it out-of-band, then apply
-- this complete migration to create the tables, functions and indexes.
CREATE EXTENSION IF NOT EXISTS pgroonga;

-- ----------------------------------------------------------------------------
-- Functions.
-- ----------------------------------------------------------------------------

-- Map a BCP-47-ish language code (e.g. "en", "es") to a Postgres regconfig.
-- Most installations only ship a subset of configs; we default to `simple`.
CREATE OR REPLACE FUNCTION searchkit_regconfig_for_language(lang text)
RETURNS regconfig
LANGUAGE sql
IMMUTABLE
AS $$
    SELECT CASE lower(trim(coalesce(lang, '')))
        WHEN 'en' THEN 'english'::regconfig
        WHEN 'es' THEN 'spanish'::regconfig
        WHEN 'fr' THEN 'french'::regconfig
        WHEN 'de' THEN 'german'::regconfig
        WHEN 'it' THEN 'italian'::regconfig
        WHEN 'pt' THEN 'portuguese'::regconfig
        WHEN 'ru' THEN 'russian'::regconfig
        -- For languages without a built-in stemmer config (e.g. ja/ko/zh),
        -- `simple` still tokenizes reasonably and is deterministic.
        ELSE 'simple'::regconfig
    END;
$$;

-- ----------------------------------------------------------------------------
-- Lexical document store (typeahead + FTS + native-script search).
-- searchkit heavy-normalizes `document` before storing; `raw_document` keeps
-- the host-provided text for FTS/PGroonga, which prefer it over `document`.
-- ----------------------------------------------------------------------------
CREATE TABLE search_documents (
    entity_type text NOT NULL,
    entity_id text NOT NULL,
    language text NOT NULL,
    document text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    raw_document text,
    tsv tsvector,
    PRIMARY KEY (entity_type, entity_id, language)
);

CREATE INDEX idx_search_documents_entity_language
    ON search_documents(entity_type, language);

-- Trigram index for typeahead.
CREATE INDEX idx_search_documents_document_gin
    ON search_documents USING gin (document gin_trgm_ops);

-- FTS index (BM25-family lexical search).
CREATE INDEX idx_search_documents_tsv_gin
    ON search_documents USING gin (tsv);

-- PGroonga full-text index for native-script queries (primary for ja/zh/ko).
-- Partial index keeps size manageable while targeting languages that most
-- need segmentation.
CREATE INDEX idx_search_documents_raw_document_pgroonga_cjk
    ON search_documents
 USING pgroonga (raw_document)
 WHERE language IN ('ja', 'zh', 'ko');

-- ----------------------------------------------------------------------------
-- Dirty queue: host marks (entity_type, entity_id, language) as changed.
-- searchkit decides what to rebuild based on runtime config.
-- ----------------------------------------------------------------------------
CREATE TABLE search_dirty (
    entity_type text NOT NULL,
    entity_id text NOT NULL,
    language text NOT NULL,
    is_deleted boolean NOT NULL DEFAULT false,
    reason text NOT NULL DEFAULT 'unknown',
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (entity_type, entity_id, language)
);

CREATE INDEX idx_search_dirty_updated_at
    ON search_dirty(updated_at);

-- Lexical backfill state (cursor-driven initial fill).
CREATE TABLE search_documents_backfill_state (
    entity_type text NOT NULL,
    language text NOT NULL,
    cursor text NOT NULL DEFAULT '',
    state text NOT NULL DEFAULT 'running', -- running|done|failed
    last_error text,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (entity_type, language)
);

CREATE INDEX idx_search_documents_backfill_state_state
    ON search_documents_backfill_state(state);


-- A timestamp is not a queue generation: now() is constant within a host
-- transaction, and delete/reinsert must not reuse an earlier generation.
ALTER TABLE search_dirty ADD COLUMN revision bigserial;

CREATE FUNCTION searchkit_bump_dirty_revision() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.revision := nextval(pg_get_serial_sequence(
        format('%I.%I', TG_TABLE_SCHEMA, TG_TABLE_NAME), 'revision'));
    RETURN NEW;
END;
$$;

-- Override explicit copies as well as ordinary host UPSERTs. Gaps are harmless.
CREATE TRIGGER search_dirty_revision
BEFORE INSERT OR UPDATE ON search_dirty
FOR EACH ROW EXECUTE FUNCTION searchkit_bump_dirty_revision();
