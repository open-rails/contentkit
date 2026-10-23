package catalog

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// retireOffers records product archival in the deletion transaction. The offer
// revision remains after physical purge, so the offers reconciler can retry
// billing independently of the content's lifetime or the event log's retention.
func (s *Store) retireOffers(ctx context.Context, tx pgx.Tx, events []Event) error {
	var contents, versions []string
	for _, e := range events {
		switch e.Type {
		case EventContentDeleted, EventContentPurged:
			contents = append(contents, e.ContentID)
		case EventVersionDeleted, EventVersionPurged:
			versions = append(versions, e.VersionID)
		}
	}
	if len(contents) == 0 && len(versions) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE `+s.t.offers+` o SET state='withdrawn', revision=revision+1, updated_at=now(),
 content_id=coalesce(o.content_id, CASE WHEN target_kind='content' THEN target_id ELSE
  (SELECT v.content_id FROM `+s.t.versions+` v WHERE v.tenant_id=o.tenant_id AND v.id=o.target_id) END)
 WHERE o.tenant_id=$1 AND o.state='active' AND
 ((o.target_kind='content' AND o.target_id=ANY($2::text[])) OR
  (o.target_kind='version' AND (o.target_id=ANY($3::text[]) OR EXISTS
   (SELECT 1 FROM `+s.t.versions+` v WHERE v.tenant_id=o.tenant_id AND v.id=o.target_id AND v.content_id=ANY($2::text[])))))`,
		s.tenant, contents, versions)
	return err
}
