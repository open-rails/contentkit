package signal

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"
)

// attributeVisits joins declared navigation identity, never a nearby timestamp
// or another visit by the same person. Receipt revisions canonicalize before
// grouping; changing editions within a visit does not multiply coverage.
func (st *Store) attributeVisits(ctx context.Context, tenant string, page *AttributionPage) error {
	table, err := ext.NewTable("contentkit_attributed_clicks", ext.Column("click_id", "String"),
		ext.Column("content_kind", "String"), ext.Column("content_id", "String"), ext.Column("subject_kind", "String"), ext.Column("subject", "String"))
	if err != nil {
		return err
	}
	byKey := map[[5]string][]*AttributedClick{}
	add := func(click *AttributedClick) error {
		key := [5]string{click.EventID, click.ContentKind, click.ContentID, click.Subject.Kind(), click.Subject.Key()}
		if len(byKey[key]) == 0 {
			if err := table.Append(key[0], key[1], key[2], key[3], key[4]); err != nil {
				return err
			}
		}
		byKey[key] = append(byKey[key], click)
		return nil
	}
	for i := range page.Renders {
		for j := range page.Renders[i].Clicks {
			if err := add(&page.Renders[i].Clicks[j]); err != nil {
				return err
			}
		}
	}
	for i := range page.Unattributed {
		if err := add(&page.Unattributed[i]); err != nil {
			return err
		}
	}
	if len(byKey) == 0 {
		return nil
	}
	ctx = clickhouse.Context(ctx, clickhouse.WithExternalTable(table))
	query := fmt.Sprintf(`WITH canonical AS (
 SELECT content_kind,content_id,content_version_id,subject_kind,subject,receipt_id,
 argMax(tuple(origin_click_id,visit_id,active_ms,covered_units,unit_attention,unit_total,unit_scale),version) AS c
 FROM %s.engagement_segments WHERE tenant=? AND %s
 AND (content_kind,content_id,subject_kind,subject) IN
 (SELECT content_kind,content_id,subject_kind,subject FROM contentkit_attributed_clicks)
 GROUP BY content_kind,content_id,content_version_id,subject_kind,subject,receipt_id
), editions AS (
 SELECT c.1 AS click_id,c.2 AS visit_id,content_kind,content_id,content_version_id,subject_kind,subject,
 sum(toUInt64(c.3)) AS active,groupUniqArrayArray(c.4) AS units,sumMap(c.5) AS attention,max(c.6) AS total,max(c.7) AS scale
 FROM canonical WHERE (c.1,content_kind,content_id,subject_kind,subject) IN
 (SELECT click_id,content_kind,content_id,subject_kind,subject FROM contentkit_attributed_clicks)
 GROUP BY click_id,visit_id,content_kind,content_id,content_version_id,subject_kind,subject
)
 SELECT click_id,content_kind,content_id,subject_kind,subject,visit_id,
 groupUniqArray(content_version_id),sum(active),
 max(least(1.,toFloat64(length(units))/greatest(toFloat64(total),1.))),
 max(toUInt8(active>=1000 AND length(units)>0 AND toFloat64(length(arrayFilter(unit -> scale=1 OR attention[intDiv(unit,scale)]>=1000,units)))/greatest(toFloat64(total),1.)>=0.9))
 FROM editions GROUP BY click_id,content_kind,content_id,subject_kind,subject,visit_id ORDER BY click_id,visit_id`, st.db, st.notErased())
	rows, err := st.conn.Query(ctx, query, tenant)
	if err != nil {
		return fmt.Errorf("signal: attributed visits: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key [5]string
		var visit AttributedVisit
		var completed uint8
		if err := rows.Scan(&key[0], &key[1], &key[2], &key[3], &key[4], &visit.VisitID, &visit.VersionIDs, &visit.ActiveMS, &visit.Coverage, &completed); err != nil {
			return err
		}
		visit.Completed = completed != 0
		for _, click := range byKey[key] {
			click.Visits = append(click.Visits, visit)
		}
	}
	return rows.Err()
}
