-- Typo matching reads the terms index; nothing orders by trigram distance.
DROP INDEX IF EXISTS content_search_documents_body_fuzzy;
