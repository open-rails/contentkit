package versions

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// The catalog calls these in its writing transaction: a target is a content
// (every version's item) or one version.

// RepublishTx queues versions' publication after their segments changed.
func (s *Service) RepublishTx(context.Context, pgx.Tx, []Target) error {
	// Access and chapters are queried live. They do not alter the immutable
	// ready-file snapshot or require media processing.
	return nil
}

// DeleteTx permanently fences targets while their catalog rows are locked.
// The caller commits this claim before Delete erases storage, so retirement
// hooks never wait on catalog locks held by the erasing transaction.
func (s *Service) DeleteTx(ctx context.Context, tx pgx.Tx, targets []Target) error {
	// A publisher locks and retires publications while it holds their
	// version: lock the versions before them, after their contents as
	// catalog writes and media callbacks do.
	contents, ids := make([]string, len(targets)), make([]string, len(targets))
	for i, t := range targets {
		contents[i], ids[i] = t.Content, t.Version
	}
	if _, err := tx.Exec(ctx, `SELECT FROM `+s.t.items+` WHERE tenant_id = $1 AND id = ANY($2) ORDER BY id FOR UPDATE`, s.o.Tenant, contents); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT FROM `+s.t.versions+` v JOIN unnest($2::text[], $3::text[]) AS t(content, version)
ON v.content_id = t.content AND (t.version = '' OR v.id = t.version) WHERE v.tenant_id = $1 ORDER BY v.id FOR UPDATE OF v`, s.o.Tenant, contents, ids); err != nil {
		return err
	}
	refs, kinds, err := s.itemRefs(ctx, tx, targets)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if err := s.m.Jobs.ClaimErasureTx(ctx, tx, ref); err != nil {
			return err
		}
	}
	for kind, versions := range kinds {
		k, err := s.reg.Kind(kind)
		if err != nil || !k.Versions {
			continue
		}
		rows, err := tx.Query(ctx, `SELECT id FROM `+s.t.publications+` WHERE tenant_id = $1 AND version_id = ANY($2)`, s.o.Tenant, versions)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if err := s.retireTx(ctx, tx, k, ids); err != nil {
			return err
		}
	}
	return nil
}

// Delete erases the already-fenced exact versions. A stable operation ID
// preserves the original cleanup targets when a purge retries after failure.
func (s *Service) Delete(ctx context.Context, targets []Target) error {
	for _, target := range targets {
		if target.Version == "" {
			return errors.New("versions: storage erasure requires exact versions")
		}
	}
	refs, _, err := s.itemRefs(ctx, s.o.Pool, targets)
	if err != nil {
		return err
	}
	// Refuse a mistaken direct call before erasing any member of the batch.
	if err := pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		for _, ref := range refs {
			claimed, err := s.m.Jobs.ErasureStartedTx(ctx, tx, ref)
			if err != nil {
				return err
			}
			if !claimed {
				return errors.New("versions: storage erasure requires a committed claim")
			}
		}
		return nil
	}); err != nil {
		return err
	}
	var failures error
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		if err := s.m.Jobs.Erase(ctx, media.Deletion{Ref: ref, OperationID: "content-purge:" + ref.Version()}); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

// CheckRestorableTx refuses a version after its permanent erasure claim,
// including a claim whose storage response or catalog transaction failed.
// Callers hold the same parent/version locks as DeleteTx.
func (s *Service) CheckRestorableTx(ctx context.Context, tx pgx.Tx, targets []Target) error {
	refs, _, err := s.itemRefs(ctx, tx, targets)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		started, err := s.m.Jobs.ErasureStartedTx(ctx, tx, ref)
		if err != nil {
			return err
		}
		if started {
			return media.ErrNotFound
		}
	}
	return nil
}

// ResumeDeletes keeps late-write cleanup in the content purge operation.
func (s *Service) ResumeDeletes(ctx context.Context, limit int) (int, error) {
	return s.m.Jobs.PurgeRetired(ctx, limit)
}

// itemRefs are targets' versions' media items, and their versions by kind.
func (s *Service) itemRefs(ctx context.Context, q querier, targets []Target) ([]contentref.ContentRef, map[string][]string, error) {
	contents, versions := make([]string, len(targets)), make([]string, len(targets))
	for i, t := range targets {
		contents[i], versions[i] = t.Content, t.Version
	}
	rows, err := q.Query(ctx, `SELECT c.kind, c.id, v.id FROM unnest($2::text[], $3::text[]) AS t(content, version)
JOIN `+s.t.items+` c ON c.tenant_id = $1 AND c.id = t.content
JOIN `+s.t.versions+` v ON v.tenant_id = $1 AND v.content_id = c.id AND (t.version = '' OR v.id = t.version)`, s.o.Tenant, contents, versions)
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
