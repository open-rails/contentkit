package discovery

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// cardStatus marks the cards whose work has a newer published version than
// theirs, or whose version gained pages after it was published, and, for
// staff and the item's creators, the deleted ones.
func (s *Service) cardStatus(ctx context.Context, rows []cardRow, staff bool) error {
	if len(rows) == 0 {
		return nil
	}
	vids := make([]string, len(rows))
	for i, c := range rows {
		vids[i] = c.Version.ID
	}
	got, err := s.o.Pool.Query(ctx, fmt.Sprintf(`SELECT p.ord, f.deleted, GREATEST(
 (SELECT max(o.published_at) FROM %[2]s o WHERE o.tenant_id = v.tenant_id AND o.content_id = v.content_id AND o.id <> v.id
  AND o.state = 'published' AND o.published_at > coalesce(v.published_at, f.published_at)),
 CASE WHEN v.pages_added_at > coalesce(v.published_at, f.published_at) THEN v.pages_added_at END)
FROM unnest($2::text[]) WITH ORDINALITY AS p(vid, ord)
JOIN %[1]s f ON f.tenant_id = $1 AND f.version_id = p.vid
JOIN %[2]s v ON v.tenant_id = f.tenant_id AND v.id = f.version_id`, s.t.facets, s.t.versions), s.o.Tenant, vids)
	if err != nil {
		return err
	}
	type status struct {
		ord     int64
		deleted bool
		updated *time.Time
	}
	marks, err := pgx.CollectRows(got, func(r pgx.CollectableRow) (status, error) {
		var st status
		err := r.Scan(&st.ord, &st.deleted, &st.updated)
		return st, err
	})
	if err != nil {
		return err
	}
	for _, st := range marks {
		c := &rows[st.ord-1]
		c.UpdatedAt = st.updated
		c.Deleted = st.deleted && (staff || c.creator)
	}
	return nil
}
