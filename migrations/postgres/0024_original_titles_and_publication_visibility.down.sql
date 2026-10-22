DROP INDEX content_version_text_management_prefix;
DROP INDEX content_version_text_management_substring;
CREATE OR REPLACE VIEW content_management_content_names AS
SELECT c.tenant_id,c.kind AS content_kind,c.id AS content_id,NULL::text AS content_version_id,t.language,t.title,t.aliases
FROM content_items c JOIN content_text t ON t.tenant_id=c.tenant_id AND t.content_id=c.id
WHERE c.state<>'deleted';
-- The retired listing classification cannot be reconstructed after adoption.
ALTER TABLE content_items ADD COLUMN listing text NOT NULL DEFAULT 'listed'
    CHECK (listing IN ('listed', 'members', 'unlisted'));
ALTER TABLE content_facets ADD COLUMN listing text NOT NULL DEFAULT 'listed';
ALTER TABLE content_items DROP COLUMN original_title, DROP COLUMN original_language;
