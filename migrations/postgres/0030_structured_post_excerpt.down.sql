ALTER TABLE content_text DROP COLUMN body_excerpt;
ALTER TABLE content_text ADD COLUMN body_excerpt text GENERATED ALWAYS AS (
  left(btrim(regexp_replace(
    replace(replace(replace(replace(replace(replace(regexp_replace(body, '<[^>]+>', ' ', 'g'),
      '&nbsp;', ' '), '&lt;', '<'), '&gt;', '>'), '&quot;', '"'), '&#39;', ''''), '&amp;', '&'),
    '\s+', ' ', 'g')), 400)) STORED;
DROP FUNCTION contentkit_body_excerpt(text);
