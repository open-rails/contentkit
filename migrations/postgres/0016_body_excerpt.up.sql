-- The body's plain-text opening, derived once at write, for post list excerpts
-- (tags dropped, common entities decoded, whitespace collapsed).
ALTER TABLE content_text ADD COLUMN IF NOT EXISTS body_excerpt text GENERATED ALWAYS AS (
  left(btrim(regexp_replace(
    replace(replace(replace(replace(replace(replace(regexp_replace(body, '<[^>]+>', ' ', 'g'),
      '&nbsp;', ' '), '&lt;', '<'), '&gt;', '>'), '&quot;', '"'), '&#39;', ''''), '&amp;', '&'),
    '\s+', ' ', 'g')), 400)) STORED;
