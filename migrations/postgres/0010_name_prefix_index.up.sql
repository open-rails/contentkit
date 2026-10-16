-- Autocomplete is a name prefix, never a full-text/contextual keyword query.
-- Term search preserves whole names in every script (including CJK), with no
-- tokenizer dependency. Reuse ContentKit's normalization for title and aliases.
CREATE INDEX content_search_documents_name_prefix ON content_search_documents
USING pgroonga (
  (contentkit_keyword_terms(title, aliases, '{}'::text[], ''))
  public.pgroonga_text_array_term_search_ops_v2
);
