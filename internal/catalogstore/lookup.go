package catalogstore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	catalog "github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
)

// LookupVersion returns its parent detail with only the requested edition.
// The parent relationship is database-owned; no media path participates in
// resolving it. Normal visibility and access rules still apply.
func (s *Store) LookupVersion(ctx context.Context, viewer *Viewer, versionID string) (*catalog.Content, error) {
	if contentref.ValidateID(versionID) != nil {
		return nil, ErrNotFound
	}
	var parent string
	err := s.pool.QueryRow(ctx, "SELECT content_id FROM "+s.t.versions+" WHERE tenant_id=$1 AND id=$2", s.tenant, versionID).Scan(&parent)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	contents, err := s.contents(ctx, viewer, []string{parent}, versionID)
	if err != nil {
		return nil, err
	}
	if len(contents) != 1 || contents[0] == nil || len(contents[0].Versions) != 1 || contents[0].Versions[0].ID != versionID {
		return nil, ErrNotFound
	}
	return contents[0], nil
}

// VersionStatuses returns the existing editions among ids in ids' order.
func (s *Store) VersionStatuses(ctx context.Context, ids []string) ([]catalog.VersionStatus, error) {
	if len(ids) == 0 || len(ids) > catalog.MaxVersionStatuses {
		return nil, invalid("ids lists 1 to %d version ids", catalog.MaxVersionStatuses)
	}
	for _, id := range ids {
		if contentref.ValidateID(id) != nil {
			return nil, invalid("ids: %q is not a version id", id)
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT v.id, v.content_id, c.kind, v.languages, d.deleted, f.live_from,
 coalesce(NOT d.deleted AND f.live_from <= $3, false)
 FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id
 CROSS JOIN LATERAL (SELECT c.state = 'deleted' OR v.state = 'deleted' AS deleted) d
 LEFT JOIN `+s.t.facets+` f ON f.tenant_id = v.tenant_id AND f.version_id = v.id
 WHERE v.tenant_id = $1 AND v.id = ANY($2::text[]) ORDER BY array_position($2::text[], v.id)`, s.tenant, ids, s.now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []catalog.VersionStatus{}
	for rows.Next() {
		var v catalog.VersionStatus
		if err := rows.Scan(&v.VersionID, &v.ContentID, &v.Kind, &v.Languages, &v.Deleted, &v.LiveFrom, &v.Live); err != nil {
			return nil, err
		}
		if v.Languages == nil {
			v.Languages = []string{}
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
