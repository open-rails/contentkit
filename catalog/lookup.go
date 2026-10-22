package catalog

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
)

// LookupVersion returns its parent detail with only the requested edition.
// The parent relationship is database-owned; no media path participates in
// resolving it. Normal visibility and access rules still apply.
func (s *Store) LookupVersion(ctx context.Context, viewer *Viewer, versionID string) (*Content, error) {
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
