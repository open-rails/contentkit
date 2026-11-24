-- A channel's authorization group is its own id: auth_scope is NULL while
-- pending, else the id. A stored scope naming another id stops granting and
-- returns to pending; the channel's next write provisions its own group.
UPDATE content_entities SET auth_scope = NULL, revision = revision + 1, updated_at = clock_timestamp()
WHERE auth_scope IS NOT NULL AND auth_scope <> id;
ALTER TABLE content_entities ADD CONSTRAINT content_entities_auth_scope_is_id CHECK (auth_scope IS NULL OR auth_scope = id);

-- IPv4-mapped networks are stored as IPv4, as addresses are matched.
UPDATE content_ip_bans SET network = set_masklen('0.0.0.0'::inet + (network - '::ffff:0.0.0.0'::inet), masklen(network) - 96)::cidr
WHERE family(network) = 6 AND network <<= '::ffff:0.0.0.0/96'::cidr
AND NOT EXISTS (SELECT 1 FROM content_ip_bans b WHERE b.tenant_id = content_ip_bans.tenant_id
 AND b.network = set_masklen('0.0.0.0'::inet + (content_ip_bans.network - '::ffff:0.0.0.0'::inet), masklen(content_ip_bans.network) - 96)::cidr);
DELETE FROM content_ip_bans WHERE family(network) = 6 AND network <<= '::ffff:0.0.0.0/96'::cidr;
UPDATE content_location_rule_entries SET network = set_masklen('0.0.0.0'::inet + (network - '::ffff:0.0.0.0'::inet), masklen(network) - 96)::cidr
WHERE family(network) = 6 AND network <<= '::ffff:0.0.0.0/96'::cidr;

-- Account erasure finds the account's media commits by actor and quota owner.
CREATE INDEX IF NOT EXISTS content_media_commits_actor ON content_media_commits (tenant_id, actor_id);
CREATE INDEX IF NOT EXISTS content_media_commits_settlement_owner ON content_media_commits (tenant_id, (effects->'settlement'->>'Owner'))
WHERE effects->'settlement'->>'Owner' <> '';
CREATE INDEX IF NOT EXISTS content_media_commits_deletion_owner ON content_media_commits (tenant_id, (effects->'folder_delete'->>'owner'))
WHERE effects->'folder_delete' ? 'owner';
