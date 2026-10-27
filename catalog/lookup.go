package catalog

import (
	"context"
	"errors"
	"time"

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

// VersionStatus is an edition's catalog state, for hosts that keep references
// to editions. A purged edition has none.
type VersionStatus struct {
	VersionID string   `json:"version_id"`
	ContentID string   `json:"content_id"`
	Kind      string   `json:"kind"`
	Languages []string `json:"languages"`
	// Deleted is the edition's or its content's deletion.
	Deleted bool `json:"deleted"`
	// Live is released and not deleted at the time of the read. No event
	// fires when a scheduled LiveFrom passes; compare it with the time.
	Live bool `json:"live"`
	// LiveFrom is when its release completed, kept through deletion; absent
	// before release.
	LiveFrom *time.Time `json:"live_from,omitempty"`
}

// MaxVersionStatuses bounds VersionStatuses.
const MaxVersionStatuses = 100

// VersionStatuses returns the existing editions among ids in ids' order.
func (s *Store) VersionStatuses(ctx context.Context, ids []string) ([]VersionStatus, error) {
	if len(ids) == 0 || len(ids) > MaxVersionStatuses {
		return nil, invalid("ids lists 1 to %d version ids", MaxVersionStatuses)
	}
	for _, id := range ids {
		if contentref.ValidateID(id) != nil {
			return nil, invalid("ids: %q is not a version id", id)
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT v.id, v.content_id, c.kind, v.languages, d.deleted, f.live_from,
 coalesce(NOT d.deleted AND f.live_from <= statement_timestamp(), false)
 FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id
 CROSS JOIN LATERAL (SELECT c.state = 'deleted' OR v.state = 'deleted' AS deleted) d
 LEFT JOIN `+s.t.facets+` f ON f.tenant_id = v.tenant_id AND f.version_id = v.id
 WHERE v.tenant_id = $1 AND v.id = ANY($2::text[]) ORDER BY array_position($2::text[], v.id)`, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VersionStatus{}
	for rows.Next() {
		var v VersionStatus
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
