package catalogstore

import (
	"context"

	"github.com/jackc/pgx/v5"
	catalog "github.com/open-rails/contentkit/catalog"
)

// deleteEmptyParents follows version deletion in the already locked content
// transaction. Independent moderation and owner deletions keep their cause.
func (s *Store) deleteEmptyParents(ctx context.Context, tx pgx.Tx, ids []string) ([]catalog.Event, error) {
	rows, err := tx.Query(ctx, `UPDATE `+s.t.items+` c SET state='deleted',deleted_at=clock_timestamp(),deleted_without_versions=true
 WHERE c.tenant_id=$1 AND c.id=ANY($2) AND c.state<>'deleted'
 AND NOT EXISTS (SELECT 1 FROM `+s.t.versions+` v WHERE v.tenant_id=c.tenant_id AND v.content_id=c.id AND v.state<>'deleted')
 RETURNING c.id,c.kind`, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (catalog.Event, error) {
		e := catalog.Event{Type: catalog.EventContentDeleted}
		err := r.Scan(&e.ContentID, &e.Kind)
		return e, err
	})
}

// restoreVersionParents revives only parents hidden because their last edition
// was removed. It cannot reverse an independent moderation or owner deletion.
func (s *Store) restoreVersionParents(ctx context.Context, tx pgx.Tx, ids []string) ([]catalog.Event, error) {
	rows, err := tx.Query(ctx, `SELECT c.id,c.kind FROM `+s.t.items+` c
 WHERE c.tenant_id=$1 AND c.id=ANY($2) AND c.state='deleted' AND c.deleted_without_versions
 AND EXISTS (SELECT 1 FROM `+s.t.versions+` v WHERE v.tenant_id=c.tenant_id AND v.content_id=c.id AND v.state<>'deleted') ORDER BY c.id`, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	events, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (catalog.Event, error) {
		e := catalog.Event{Type: catalog.EventContentRestored}
		err := r.Scan(&e.ContentID, &e.Kind)
		return e, err
	})
	if err != nil || len(events) == 0 {
		return events, err
	}
	parents := make([]string, len(events))
	for i, e := range events {
		parents[i] = e.ContentID
	}
	return events, s.restoreRows(ctx, tx, parents)
}
