DELETE FROM content_poll_votes v
USING content_poll_questions q, content_poll_options o
WHERE q.deleted_at IS NOT NULL
  AND o.tenant_id = q.tenant_id AND o.question_id = q.id
  AND v.tenant_id = o.tenant_id AND v.option_id = o.id;
