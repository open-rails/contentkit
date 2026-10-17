package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

const measuredKeyFilter = `(content_kind,content_id,subject_kind,subject) IN
 (SELECT content_kind,content_id,subject_kind,subject FROM contentkit_projection_keys)`

func (st *Store) recordMeasured(ctx context.Context, tenant string, events []Signal) error {
	if len(events) == 0 {
		return nil
	}
	subjects := make([]Subject, len(events))
	for i, s := range events {
		if err := s.validate(tenant); err != nil {
			return err
		}
		if s.Measurement == nil {
			return fmt.Errorf("signal: measured consumption requires a measurement")
		}
		subjects[i] = s.Subject
	}
	fenced, err := st.fenced(ctx, tenant, subjects)
	if err != nil {
		return err
	}
	keys := map[ProjectionKey]struct{}{}
	for _, s := range events {
		if _, erased := fenced[[2]string{s.Subject.Kind(), s.Subject.Key()}]; erased {
			continue
		}
		// Preview/unknown access is not full-content engagement evidence.
		if s.Type == TypeEngagement && (s.Access == nil || !s.Access.ObservedAt.Equal(s.OccurredAt)) {
			return fmt.Errorf("signal: measured consumption needs its immutable trusted access time")
		}
		if s.Access == nil || s.Access.ViewerAccess != "full" || s.Type != TypeEngagement {
			continue
		}
		m := s.Measurement
		if m.VersionID == "" || m.VersionID != s.Version() || m.VisitID == "" || m.SegmentID == "" || s.Revision == 0 {
			return fmt.Errorf("signal: measured segment needs its immutable version, visit and revision")
		}
		if err := checkIdentifiers("VisitID", m.VisitID, "SegmentID", m.SegmentID); err != nil {
			return err
		}
		var normalized Measurement
		switch m.Scale {
		case 100:
			if m.Total%100 != 0 {
				return fmt.Errorf("signal: invalid page measurement denominator")
			}
			normalized, err = MeasurePages(m.Pages, m.Total/100, m.ActiveMS)
		case 1:
			normalized, err = MeasurePlayback(m.Watched, m.Total*1000, m.ActiveMS)
		default:
			return fmt.Errorf("signal: invalid measurement scale")
		}
		if err != nil {
			return err
		}
		if normalized.Total != m.Total || normalized.Scale != m.Scale || !slices.Equal(normalized.Units, m.Units) || !slices.Equal(normalized.Attention, m.Attention) {
			return fmt.Errorf("signal: normalized measurement differs from its source")
		}
		accessJSON, err := json.Marshal(s.Access)
		if err != nil {
			return err
		}
		// Preserve precise source geometry without duplicating the expanded units.
		raw, err := json.Marshal(struct {
			Pages   []PageEngagement `json:"pages,omitempty"`
			Watched []WatchedRange   `json:"watched,omitempty"`
		}{m.Pages, m.Watched})
		if err != nil {
			return err
		}
		attentionKeys, attentionMS := make([]uint32, 0, len(m.Attention)), make([]uint64, 0, len(m.Attention))
		for _, pair := range m.Attention {
			attentionKeys = append(attentionKeys, uint32(pair[0]))
			attentionMS = append(attentionMS, pair[1])
		}
		// One bounded snapshot per statement stays below the driver's inlined SQL
		// limit even for the maximum 128-page geometry. Nothing is incremented.
		q := fmt.Sprintf(`INSERT INTO %s.engagement_segments
 (tenant,content_kind,content_id,content_version_id,subject_kind,subject,receipt_id,revision,visit_id,segment_id,
 occurred_at,active_ms,covered_units,unit_attention,unit_total,unit_scale,resume,access_context,measurement)
 SELECT tenant,content_kind,content_id,content_version_id,subject_kind,subject,receipt_id,revision,visit_id,segment_id,
 occurred_at,active_ms,covered_units,unit_attention,unit_total,unit_scale,resume,access_context,measurement
 FROM (SELECT ? AS tenant,? AS content_kind,? AS content_id,? AS content_version_id,? AS subject_kind,? AS subject,
 ? AS receipt_id,toUInt64(?) AS revision,? AS visit_id,? AS segment_id,toDateTime64(?,6,'UTC') AS occurred_at,
 toUInt32(?) AS active_ms,CAST(?,'Array(UInt32)') AS covered_units,
 mapFromArrays(CAST(?,'Array(UInt32)'),CAST(?,'Array(UInt64)')) AS unit_attention,
 toUInt32(?) AS unit_total,toUInt32(?) AS unit_scale,? AS resume,? AS access_context,? AS measurement)
 WHERE %s`, st.db, st.notErased())
		if err := st.conn.Exec(ctx, q, tenant, s.ContentKind, s.ContentID, s.Version(), s.Subject.Kind(), s.Subject.Key(), s.EventID, s.Revision, m.VisitID, m.SegmentID,
			s.OccurredAt.UTC().Format("2006-01-02 15:04:05.000000"), m.ActiveMS, m.Units, attentionKeys, attentionMS, m.Total, m.Scale, s.Resume, string(accessJSON), string(raw)); err != nil {
			return fmt.Errorf("signal: insert measured segment: %w", err)
		}
		keys[ProjectionKey{ContentKey: s.Key(), Subject: s.Subject}] = struct{}{}
	}
	ordered := make([]ProjectionKey, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	return st.projectMeasured(ctx, tenant, ordered)
}

// measuredCanonical fixes one winning cumulative snapshot per receipt before
// any sum/union. Revisiting a work creates measurements, never a new counter.
func (st *Store) measuredCanonical(filter string) string {
	return fmt.Sprintf(`SELECT %s,subject_kind,subject,receipt_id,
 argMax(tuple(occurred_at,active_ms,covered_units,unit_attention,unit_total,unit_scale,resume,revision),version) AS c,
 max(version) AS ver
 FROM %s.engagement_segments WHERE tenant=? AND %s AND %s
 GROUP BY %s,subject_kind,subject,receipt_id`, refColumns, st.db, filter, st.notErased(), refColumns)
}

// measuredVersions is shared by projections and window ranking. Quality is
// capped per person and logical page; repeat visits do not create extra votes.
func measuredVersions(canonical string) string {
	return `WITH canonical AS (` + canonical + `),
 measured AS (SELECT ` + refColumns + `,subject_kind,subject,
 min(c.1) AS first_at,max(c.1) AS last_at,sum(toUInt64(c.2)) AS active_ms,
 groupUniqArrayIf(toDate(c.1),c.2>0) AS days,
 groupUniqArrayArray(c.3) AS units,sumMap(c.4) AS attention,max(c.5) AS total,max(c.6) AS scale,
 argMax(c.7,tuple(c.1,c.8,receipt_id)) AS resume,sum(toUInt256(ver)+1) AS generation
 FROM canonical GROUP BY ` + refColumns + `,subject_kind,subject),
 scored AS (SELECT *,active_ms>=1000 AND length(units)>0 AS qualified,
 least(1.,toFloat64(length(units))/greatest(toFloat64(total),1.)) AS coverage,
 arrayFilter(unit -> scale=1 OR attention[intDiv(unit,scale)]>=1000,units) AS read_units,
 arrayReduce('sumMap',[arrayMap(unit -> intDiv(unit,scale),units)],[arrayMap(unit -> toUInt64(1),units)]) AS page_cells,
 if(scale=1,coverage,
  arraySum(arrayMap((page,cells)->least(toFloat64(cells)/scale,toFloat64(attention[page])/8000.),page_cells.1,page_cells.2))
  /greatest(toFloat64(total)/scale,1.)) AS attention_quality,
 qualified AND toFloat64(length(read_units))/greatest(toFloat64(total),1.)>=0.9 AS completed
 FROM measured)`
}

func (st *Store) projectMeasured(ctx context.Context, tenant string, keys []ProjectionKey) error {
	if len(keys) == 0 {
		return nil
	}
	slices.SortFunc(keys, func(a, b ProjectionKey) int {
		if a.less(b) {
			return -1
		}
		if b.less(a) {
			return 1
		}
		return 0
	})
	ctx, err := projectionKeyContext(ctx, keys)
	if err != nil {
		return err
	}
	canonical := st.measuredCanonical(measuredKeyFilter)
	state := fmt.Sprintf(`INSERT INTO %s.engagement_state
 (tenant,subject_kind,subject,%s,source_version,first_engaged_at,last_engaged_at,active_ms,
 covered_units,unit_attention,unit_total,unit_scale,qualified,coverage,quality,completed,resume,version)
 %s
 SELECT ?,subject_kind,subject,%s,content_version_id,first_at,last_at,active_ms,
 units,attention,total,scale,qualified,coverage,attention_quality,completed,resume,generation FROM scored
 UNION ALL
 SELECT ?,subject_kind,subject,content_kind,content_id,'',
 argMax(content_version_id,tuple(coverage,last_at,content_version_id)),min(first_at),max(last_at),sum(active_ms),
 argMax(units,tuple(coverage,last_at,content_version_id)),argMax(attention,tuple(coverage,last_at,content_version_id)),
 argMax(total,tuple(coverage,last_at,content_version_id)),argMax(scale,tuple(coverage,last_at,content_version_id)),
 sum(active_ms)>=1000 AND sum(length(units))>0,max(coverage),max(attention_quality),max(completed),
 argMax(resume,tuple(last_at,content_version_id)),sum(generation)
 FROM scored GROUP BY subject_kind,subject,content_kind,content_id`, st.db, refColumns, measuredVersions(canonical), refColumns)
	if err := st.conn.Exec(ctx, state, tenant, tenant, tenant); err != nil {
		return fmt.Errorf("signal: project measured state: %w", err)
	}
	// Every day carries its key's complete generation. Receipts keep their
	// observation time immutable; existing days are nevertheless zeroed on repair
	// when an operator corrects or removes source data.
	daily := fmt.Sprintf(`INSERT INTO %s.engagement_daily
 (tenant,%s,subject_kind,subject,day,present,active_ms,covered_units,unit_attention,unit_total,unit_scale,version)
 SELECT * FROM (
 WITH canonical AS (%s)
 SELECT ? AS tenant,%s,subject_kind,subject,day,max(is_source)>0,sum(raw_active),groupUniqArrayArray(raw_units),
 sumMap(raw_attention),max(raw_total),max(raw_scale),
 sum(sum(generation)) OVER (PARTITION BY %s,subject_kind,subject)
 FROM (
 SELECT %s,subject_kind,subject,toDate(c.1) AS day,toUInt8(1) AS is_source,toUInt64(c.2) AS raw_active,
 c.3 AS raw_units,c.4 AS raw_attention,c.5 AS raw_total,c.6 AS raw_scale,toUInt256(ver)+1 AS generation FROM canonical
 UNION ALL
 SELECT %s,subject_kind,subject,day,toUInt8(0),toUInt64(0),CAST([],'Array(UInt32)'),
 CAST(map(),'Map(UInt32, UInt64)'),toUInt32(0),toUInt32(0),toUInt256(0)
 FROM %s.engagement_daily FINAL WHERE tenant=? AND %s AND %s)
 GROUP BY %s,subject_kind,subject,day)
 WHERE toYYYYMM(day) IN ?`, st.db, refColumns, canonical, refColumns, refColumns, refColumns, refColumns, st.db, measuredKeyFilter, st.notErased(), refColumns)
	months, err := st.measuredMonths(ctx, tenant)
	if err != nil {
		return err
	}
	for start := 0; start < len(months); start += 100 {
		batch := months[start:min(start+100, len(months))]
		if err := st.conn.Exec(ctx, daily, tenant, tenant, tenant, batch); err != nil {
			return fmt.Errorf("signal: project measured days: %w", err)
		}
	}
	return nil
}

func (st *Store) measuredMonths(ctx context.Context, tenant string) ([]uint32, error) {
	q := fmt.Sprintf(`SELECT DISTINCT month FROM (
 SELECT toYYYYMM(occurred_at) AS month FROM %s.engagement_segments WHERE tenant=? AND %s AND %s
 UNION ALL
 SELECT toYYYYMM(day) AS month FROM %s.engagement_daily FINAL WHERE tenant=? AND %s AND %s AND present)
 ORDER BY month`, st.db, measuredKeyFilter, st.notErased(), st.db, measuredKeyFilter, st.notErased())
	rows, err := st.conn.Query(ctx, q, tenant, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uint32
	for rows.Next() {
		var month uint32
		if err := rows.Scan(&month); err != nil {
			return nil, err
		}
		out = append(out, month)
	}
	return out, rows.Err()
}
