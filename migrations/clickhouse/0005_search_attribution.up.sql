-- Query context belongs to an existing result exposure, not a second event log.
-- Analytics query context follows the durable engagement/exposure lifecycle;
-- the separate recent-search autocomplete history keeps its own expiry.
ALTER TABLE exposures {{ON_CLUSTER}} ADD COLUMN IF NOT EXISTS search_query String DEFAULT '';
ALTER TABLE exposures {{ON_CLUSTER}} MODIFY COLUMN version UInt128 DEFAULT bitOr(bitShiftLeft(toUInt128(revision), 64), toUInt128(cityHash64(query_id, surface, ranker, language, subject_kind, subject, content_kinds, content_ids, content_version_ids, positions, occurred_at, access_contexts, search_query)));
ALTER TABLE engagement_segments {{ON_CLUSTER}} ADD COLUMN IF NOT EXISTS origin_click_id String DEFAULT '';
ALTER TABLE engagement_segments {{ON_CLUSTER}} MODIFY COLUMN version UInt128 DEFAULT bitOr(bitShiftLeft(toUInt128(revision),64),toUInt128(cityHash64(visit_id,segment_id,occurred_at,active_ms,covered_units,unit_attention,unit_total,unit_scale,resume,access_context,measurement,origin_click_id)));
