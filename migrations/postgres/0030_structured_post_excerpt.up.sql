-- Keep automatic summaries in the stored projection: list requests never need
-- to fetch a whole post. Quill bodies contain text inserts and non-text embeds;
-- only text belongs in the excerpt. The source body remains unchanged.
CREATE FUNCTION contentkit_body_excerpt(value text) RETURNS text
    LANGUAGE plpgsql IMMUTABLE STRICT
AS $$
DECLARE
    document jsonb;
    plain text := value;
    structured boolean := false;
BEGIN
    IF left(ltrim(value, E' \t\n\r'), 1) = '{' THEN
        BEGIN
            document := value::jsonb;
        EXCEPTION WHEN data_exception THEN
            -- Ordinary prose may start with a brace without being JSON.
            document := NULL;
        END;
        IF jsonb_typeof(document->'ops') = 'array' THEN
            SELECT coalesce(string_agg(
                CASE WHEN jsonb_typeof(op->'insert') = 'string'
                    THEN op->>'insert' ELSE ' ' END, '' ORDER BY ordinal), '')
            INTO plain
            FROM jsonb_array_elements(document->'ops') WITH ORDINALITY AS entries(op, ordinal);
            structured := true;
        END IF;
    END IF;
    IF NOT structured THEN
        plain := replace(replace(replace(replace(replace(replace(
            regexp_replace(plain, '<[^>]+>', ' ', 'g'),
            '&nbsp;', ' '), '&lt;', '<'), '&gt;', '>'), '&quot;', '"'), '&#39;', ''''), '&amp;', '&');
    END IF;
    RETURN left(btrim(regexp_replace(plain, '\s+', ' ', 'g')), 400);
END
$$;

ALTER TABLE content_text DROP COLUMN body_excerpt;
ALTER TABLE content_text ADD COLUMN body_excerpt text
    GENERATED ALWAYS AS (contentkit_body_excerpt(body)) STORED;
