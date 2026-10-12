-- ContentKit #117: groups (0014) replace the taxonomy tables.
DROP TABLE content_assignments, content_edges, content_node_counts, content_node_names, content_nodes;

-- Windowed memberships (featured rails), read by the window refresh job.
CREATE INDEX content_memberships_window_idx ON content_memberships (tenant_id, active_from, active_until) WHERE ((active_from IS NOT NULL) OR (active_until IS NOT NULL));
