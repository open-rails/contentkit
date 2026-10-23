-- Restore the older relational names. Derived counts are rebuilt by older code.
ALTER TABLE content_entities RENAME CONSTRAINT content_entities_pkey TO content_groups_pkey;
ALTER TABLE content_entities RENAME CONSTRAINT content_entities_attributes_check TO content_groups_attributes_check;
ALTER TABLE content_entities RENAME CONSTRAINT content_entities_ids_check TO content_groups_ids_check;
ALTER TABLE content_entities RENAME CONSTRAINT content_entities_state_check TO content_groups_state_check;
ALTER TABLE content_entity_names RENAME CONSTRAINT content_entity_names_pkey TO content_group_names_pkey;
ALTER TABLE content_entity_names RENAME CONSTRAINT content_entity_names_check TO content_group_names_check;
ALTER TABLE content_entity_names RENAME CONSTRAINT content_entity_names_kind_check TO content_group_names_kind_check;
ALTER TABLE content_entity_names RENAME CONSTRAINT content_entity_names_tenant_id_entity_id_fkey TO content_group_names_tenant_id_group_id_fkey;
ALTER TABLE content_entity_text RENAME CONSTRAINT content_entity_text_pkey TO content_group_text_pkey;
ALTER TABLE content_entity_text RENAME CONSTRAINT content_entity_text_tenant_id_entity_id_fkey TO content_group_text_tenant_id_group_id_fkey;
ALTER TABLE content_entity_edges RENAME CONSTRAINT content_entity_edges_pkey TO content_group_edges_pkey;
ALTER TABLE content_entity_edges RENAME CONSTRAINT content_entity_edges_check TO content_group_edges_check;
ALTER TABLE content_entity_edges RENAME CONSTRAINT content_entity_edges_tenant_id_from_id_fkey TO content_group_edges_tenant_id_from_id_fkey;
ALTER TABLE content_entity_edges RENAME CONSTRAINT content_entity_edges_tenant_id_to_id_fkey TO content_group_edges_tenant_id_to_id_fkey;
ALTER TABLE content_memberships RENAME CONSTRAINT content_memberships_tenant_id_entity_id_fkey TO content_memberships_tenant_id_group_id_fkey;
ALTER INDEX content_items_deleted_with_entity_idx RENAME TO content_items_deleted_with_group_idx;
ALTER INDEX content_items_owner_entity_idx RENAME TO content_items_owner_group_idx;
ALTER INDEX content_entities_kind_idx RENAME TO content_groups_kind_idx;
ALTER INDEX content_entities_owner_idx RENAME TO content_groups_owner_idx;
ALTER INDEX content_entities_parent_idx RENAME TO content_groups_parent_idx;
ALTER INDEX content_entities_purge_idx RENAME TO content_groups_purge_idx;
ALTER INDEX content_entities_slug_idx RENAME TO content_groups_slug_idx;
ALTER INDEX content_entity_names_canonical_idx RENAME TO content_group_names_canonical_idx;
ALTER INDEX content_entity_names_lookup_idx RENAME TO content_group_names_lookup_idx;
ALTER INDEX content_entity_edges_to_idx RENAME TO content_group_edges_to_idx;
ALTER INDEX content_memberships_entity_idx RENAME TO content_memberships_group_idx;
ALTER INDEX content_facets_entities_idx RENAME TO content_facets_groups_idx;
ALTER INDEX content_entity_names_management_prefix RENAME TO content_group_names_management_prefix;
ALTER INDEX content_entity_names_management_substring RENAME TO content_group_names_management_substring;
ALTER TRIGGER content_entities_deleted ON content_entities RENAME TO content_groups_deleted;
ALTER FUNCTION contentkit_entity_deleted() RENAME TO contentkit_group_deleted;
ALTER FUNCTION content_current_entity_ids(text,text,text) RENAME TO content_current_group_ids;
ALTER FUNCTION content_keyword_entity_ids(text,text) RENAME TO content_keyword_group_ids;
ALTER VIEW content_current_facets RENAME COLUMN access_entity_id TO access_group_id;
ALTER VIEW content_current_facets RENAME COLUMN entity_ids TO group_ids;
ALTER VIEW content_current_facets RENAME COLUMN owner_entity_id TO owner_group_id;
ALTER TABLE content_facets RENAME COLUMN access_entity_id TO access_group_id;
ALTER TABLE content_facets RENAME COLUMN entity_ids TO group_ids;
ALTER TABLE content_facets RENAME COLUMN owner_entity_id TO owner_group_id;
ALTER TABLE content_items RENAME COLUMN owner_entity_id TO owner_group_id;
ALTER TABLE content_items RENAME COLUMN deleted_with_entity TO deleted_with_group;
ALTER TABLE content_publishing_operations RENAME COLUMN entity_ids TO group_ids;
ALTER TABLE content_memberships RENAME COLUMN entity_id TO group_id;
ALTER TABLE content_entity_text RENAME COLUMN entity_id TO group_id;
ALTER TABLE content_entity_names RENAME COLUMN entity_id TO group_id;
ALTER TABLE content_entity_edges RENAME TO content_group_edges;
ALTER TABLE content_entity_text RENAME TO content_group_text;
ALTER TABLE content_entity_names RENAME TO content_group_names;
ALTER TABLE content_entities RENAME TO content_groups;
CREATE TABLE content_group_counts (
    tenant_id text NOT NULL,
    group_id text NOT NULL,
    content_kind text NOT NULL,
    language text NOT NULL,
    content_count integer NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT content_group_counts_pkey PRIMARY KEY (tenant_id, group_id, content_kind, language),
    CONSTRAINT content_group_counts_tenant_id_group_id_fkey FOREIGN KEY (tenant_id, group_id) REFERENCES content_groups(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX content_group_counts_rank_idx ON content_group_counts USING btree (tenant_id, content_kind, language, content_count DESC);
