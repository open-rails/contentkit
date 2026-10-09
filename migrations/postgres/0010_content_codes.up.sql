-- parent: 9 sha256:69d6084e3d17bdf1469034caa456e2c33c36de1b6fbc47d77677f31a08d9adde

-- Content codes: the short, random, permanent id a content URL carries
-- (/{route}/{CODE}/{slug}). Nine uppercase Crockford base32 characters with at
-- least one letter (an all-digit code would read as a legacy numeric id),
-- unique per tenant across every content kind, assigned once and never reused.
-- slug is the default URL slug, slugs the per-language ones; merged_into is the
-- code this content now redirects to (kept one hop deep).
CREATE FUNCTION contentkit_content_code() RETURNS text
    LANGUAGE plpgsql VOLATILE PARALLEL SAFE
    AS $$
DECLARE
    b bytea;
    c text;
    i int;
BEGIN
    LOOP
        b := uuid_send(gen_random_uuid());
        c := '';
        -- Bytes 6 and 8 carry the UUID version and variant; the other nine are random.
        FOREACH i IN ARRAY ARRAY[0, 1, 2, 3, 4, 5, 9, 10, 11] LOOP
            c := c || substr('0123456789ABCDEFGHJKMNPQRSTVWXYZ', (get_byte(b, i) & 31) + 1, 1);
        END LOOP;
        IF c ~ '[A-Z]' THEN
            RETURN c;
        END IF;
    END LOOP;
END;
$$;

CREATE TABLE content_codes (
    tenant_id text NOT NULL,
    code text NOT NULL,
    content_kind text NOT NULL,
    content_id text NOT NULL,
    slug text DEFAULT ''::text NOT NULL,
    slugs jsonb DEFAULT '{}'::jsonb NOT NULL,
    merged_into text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_codes_pkey PRIMARY KEY (tenant_id, code),
    CONSTRAINT content_codes_ref_key UNIQUE (tenant_id, content_kind, content_id),
    CONSTRAINT content_codes_code_ck CHECK (code ~ '^[0-9A-HJKMNP-TV-Z]{9}$' AND code ~ '[A-Z]'),
    CONSTRAINT content_codes_ref_ck CHECK (tenant_id <> '' AND content_kind <> '' AND content_id <> '' AND content_id = lower(content_id)),
    CONSTRAINT content_codes_slug_ck CHECK (slug ~ '^([a-z0-9]+(-[a-z0-9]+)*)?$' AND length(slug) <= 80),
    CONSTRAINT content_codes_slugs_ck CHECK (jsonb_typeof(slugs) = 'object'),
    CONSTRAINT content_codes_merged_ck CHECK (merged_into IS DISTINCT FROM code),
    CONSTRAINT content_codes_merged_into_fkey FOREIGN KEY (tenant_id, merged_into) REFERENCES content_codes(tenant_id, code)
);

CREATE INDEX content_codes_merged_into ON content_codes USING btree (tenant_id, merged_into) WHERE (merged_into IS NOT NULL);

-- Identifiers of another system (a legacy site's ids, tokens or names) that
-- resolve to a code, written once by an import: source names the system
-- ("doujins-legacy"), legacy_kind its identifier space ("folder",
-- "tag-name"), legacy_key the identifier as the host normalizes it. locator
-- is an optional host position inside the content (a page).
CREATE TABLE content_code_aliases (
    tenant_id text NOT NULL,
    source text NOT NULL,
    legacy_kind text NOT NULL,
    legacy_key text NOT NULL,
    code text NOT NULL,
    locator text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_code_aliases_pkey PRIMARY KEY (tenant_id, source, legacy_kind, legacy_key),
    CONSTRAINT content_code_aliases_ck CHECK (source ~ '^[a-z0-9][a-z0-9_.-]{0,63}$' AND legacy_kind ~ '^[a-z0-9][a-z0-9_.-]{0,63}$'
        AND legacy_key <> '' AND length(legacy_key) <= 512 AND locator <> ''),
    CONSTRAINT content_code_aliases_code_fkey FOREIGN KEY (tenant_id, code) REFERENCES content_codes(tenant_id, code)
);

-- Backfill ContentKit's own records: taxonomy nodes (slugs from their
-- canonical names, English first) and live posts. The slug expression
-- approximates contenturl.Slugify; the next write recomputes it in Go.
CREATE FUNCTION pg_temp.contentkit_slug(v text) RETURNS text
    LANGUAGE sql IMMUTABLE
    AS $$
SELECT CASE WHEN length(s) <= 80 THEN s
    WHEN strpos(left(s, 81), '-') > 0 THEN rtrim(regexp_replace(left(s, 81), '-[^-]*$', ''), '-')
    ELSE left(s, 80) END
FROM (SELECT trim(BOTH '-' FROM regexp_replace(lower(regexp_replace(regexp_replace(
    normalize(coalesce(v, ''), NFKD), '[̀-ͯ]', '', 'g'), '[''‘’ʼ]', '', 'g')),
    '[^a-z0-9]+', '-', 'g')) AS s) x
$$;

CREATE TEMP TABLE contentkit_code_backfill ON COMMIT DROP AS
SELECT n.tenant_id, n.kind AS content_kind, n.taxonomy_id AS content_id,
    coalesce((SELECT pg_temp.contentkit_slug(m.name) FROM content_node_names m
        WHERE m.tenant_id = n.tenant_id AND m.taxonomy_id = n.taxonomy_id AND m.kind = 'name'
        ORDER BY m.language = 'en' DESC, m.language LIMIT 1), '') AS slug,
    coalesce((SELECT jsonb_object_agg(m.language, pg_temp.contentkit_slug(m.name)) FROM content_node_names m
        WHERE m.tenant_id = n.tenant_id AND m.taxonomy_id = n.taxonomy_id AND m.kind = 'name'
        AND pg_temp.contentkit_slug(m.name) <> ''), '{}'::jsonb) AS slugs
FROM content_nodes n
WHERE n.tenant_id <> '' AND n.taxonomy_id = lower(n.taxonomy_id)
UNION ALL
SELECT p.tenant_id, 'post', p.id, pg_temp.contentkit_slug(coalesce(p.slug, p.title)), '{}'::jsonb
FROM content_posts p
WHERE p.deleted_at IS NULL AND p.tenant_id <> '' AND p.id = lower(p.id);

DO $$
DECLARE
    attempt int := 0;
BEGIN
    LOOP
        INSERT INTO content_codes (tenant_id, code, content_kind, content_id, slug, slugs)
        SELECT b.tenant_id, contentkit_content_code(), b.content_kind, b.content_id, b.slug, b.slugs
        FROM contentkit_code_backfill b
        WHERE NOT EXISTS (SELECT 1 FROM content_codes c
            WHERE c.tenant_id = b.tenant_id AND c.content_kind = b.content_kind AND c.content_id = b.content_id)
        ON CONFLICT DO NOTHING;
        EXIT WHEN NOT EXISTS (SELECT 1 FROM contentkit_code_backfill b WHERE NOT EXISTS (SELECT 1 FROM content_codes c
            WHERE c.tenant_id = b.tenant_id AND c.content_kind = b.content_kind AND c.content_id = b.content_id));
        attempt := attempt + 1;
        IF attempt >= 16 THEN
            RAISE EXCEPTION 'contentkit: content code backfill did not converge';
        END IF;
    END LOOP;
END;
$$;

-- A merged node redirects to its survivor, followed to the end of the chain.
UPDATE content_codes c SET merged_into = t.code
FROM content_nodes n
JOIN content_edges e ON e.tenant_id = n.tenant_id AND e.from_taxonomy_id = n.taxonomy_id AND e.relation = 'alias_of'
JOIN content_nodes s ON s.tenant_id = e.tenant_id AND s.taxonomy_id = e.to_taxonomy_id
JOIN content_codes t ON t.tenant_id = s.tenant_id AND t.content_kind = s.kind AND t.content_id = s.taxonomy_id
WHERE n.state = 'merged' AND c.tenant_id = n.tenant_id AND c.content_kind = n.kind AND c.content_id = n.taxonomy_id;

DO $$
BEGIN
    FOR i IN 1..16 LOOP
        UPDATE content_codes c SET merged_into = t.merged_into
        FROM content_codes t
        WHERE c.tenant_id = t.tenant_id AND c.merged_into = t.code AND t.merged_into IS NOT NULL AND t.merged_into <> c.code;
        EXIT WHEN NOT FOUND;
    END LOOP;
END;
$$;

DROP FUNCTION pg_temp.contentkit_slug(text);
