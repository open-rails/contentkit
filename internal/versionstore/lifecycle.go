package versionstore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	versions "github.com/open-rails/contentkit/media/versions"
)

// DeleteTx claims targets' media erasure while their catalog rows are
// locked: purge_claimed_at, which a restore checks under the same locks and
// refuses from then on. It touches no storage; Delete erases the bytes after
// the caller commits.
func (s *Service) DeleteTx(ctx context.Context, tx pgx.Tx, targets []versions.Target) error {
	// Lock the versions after their contents, as catalog writes do.
	contents, ids := make([]string, len(targets)), make([]string, len(targets))
	for i, t := range targets {
		contents[i], ids[i] = t.Content, t.Version
	}
	if _, err := tx.Exec(ctx, `SELECT FROM `+s.t.items+` WHERE tenant_id = $1 AND id = ANY($2) ORDER BY id FOR UPDATE`, s.o.Tenant, contents); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE `+s.t.versions+` v SET purge_claimed_at = coalesce(v.purge_claimed_at, now())
FROM (SELECT v.id FROM `+s.t.versions+` v JOIN unnest($2::text[], $3::text[]) AS t(content, version)
 ON v.content_id = t.content AND (t.version = '' OR v.id = t.version) WHERE v.tenant_id = $1 ORDER BY v.id FOR UPDATE OF v) claimed
WHERE v.tenant_id = $1 AND v.id = claimed.id`, s.o.Tenant, contents, ids)
	return err
}

// Delete erases the claimed exact versions' media: each item's permanent
// fence, tombstone manifest and bytes. Retrying repeats it.
func (s *Service) Delete(ctx context.Context, targets []versions.Target) error {
	for _, target := range targets {
		if target.Version == "" {
			return errors.New("versions: storage erasure requires exact versions")
		}
	}
	// Refuse a mistaken direct call before erasing any member of the batch.
	vers := make([]string, len(targets))
	for i, t := range targets {
		vers[i] = t.Version
	}
	var claimed int
	if err := s.o.Pool.QueryRow(ctx, `SELECT count(*) FROM `+s.t.versions+` WHERE tenant_id = $1 AND id = ANY($2) AND purge_claimed_at IS NOT NULL`,
		s.o.Tenant, vers).Scan(&claimed); err != nil {
		return err
	}
	if claimed != len(targets) {
		return errors.New("versions: storage erasure requires a committed claim")
	}
	refs, _, err := s.itemRefs(ctx, s.o.Pool, targets)
	if err != nil {
		return err
	}
	var failures error
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		if err := s.m.Jobs.Erase(ctx, ref); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

// CheckRestorableTx refuses a version whose erasure was claimed, or whose
// media was erased. Callers hold the same parent/version locks as DeleteTx.
func (s *Service) CheckRestorableTx(ctx context.Context, tx pgx.Tx, targets []versions.Target) error {
	contents, vers := make([]string, len(targets)), make([]string, len(targets))
	for i, t := range targets {
		contents[i], vers[i] = t.Content, t.Version
	}
	var claimed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.t.versions+` v JOIN unnest($2::text[], $3::text[]) AS t(content, version)
 ON v.content_id = t.content AND (t.version = '' OR v.id = t.version) WHERE v.tenant_id = $1 AND v.purge_claimed_at IS NOT NULL)`,
		s.o.Tenant, contents, vers).Scan(&claimed); err != nil {
		return err
	}
	if claimed {
		return mediastore.ErrNotFound
	}
	refs, _, err := s.itemRefs(ctx, tx, targets)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		erased, err := s.m.Jobs.Erased(ctx, ref)
		if err != nil {
			return err
		}
		if erased {
			return mediastore.ErrNotFound
		}
	}
	return nil
}

// ResumeDeletes has nothing to drain: each erased item's garbage collection
// takes what lands after its erasure.
func (s *Service) ResumeDeletes(ctx context.Context, limit int) (int, error) { return 0, nil }

// itemRefs are targets' versions' media items, and their versions by kind.
func (s *Service) itemRefs(ctx context.Context, q querier, targets []versions.Target) ([]contentref.ContentRef, map[string][]string, error) {
	contents, vers := make([]string, len(targets)), make([]string, len(targets))
	for i, t := range targets {
		contents[i], vers[i] = t.Content, t.Version
	}
	rows, err := q.Query(ctx, `SELECT c.kind, c.id, v.id FROM unnest($2::text[], $3::text[]) AS t(content, version)
JOIN `+s.t.items+` c ON c.tenant_id = $1 AND c.id = t.content
JOIN `+s.t.versions+` v ON v.tenant_id = $1 AND v.content_id = c.id AND (t.version = '' OR v.id = t.version)`, s.o.Tenant, contents, vers)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var refs []contentref.ContentRef
	byKind := map[string][]string{}
	seen := map[contentref.ContentKey]bool{}
	add := func(kind, id, version string) {
		ref, err := s.reg.VersionRef(kind, id, version)
		if err == nil && !seen[ref.Key()] {
			seen[ref.Key()] = true
			refs = append(refs, ref)
		}
	}
	for rows.Next() {
		var kind, id, version string
		if err := rows.Scan(&kind, &id, &version); err != nil {
			return nil, nil, err
		}
		add(kind, id, version)
		byKind[kind] = append(byKind[kind], version)
	}
	return refs, byKind, rows.Err()
}
