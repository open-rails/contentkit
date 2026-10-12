package versions

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// The catalog calls these in its writing transaction: a target is a content
// (its item and every version's) or one version.

// ExposeTx brings targets' media to their visibility once it changed:
// published, unpublished, deleted, or due.
func (s *Service) ExposeTx(ctx context.Context, tx pgx.Tx, targets []Target) error {
	refs, _, err := s.itemRefs(ctx, tx, targets)
	if err != nil {
		return err
	}
	return s.m.Jobs.ExposeTx(ctx, tx, refs...)
}

// RepublishTx queues versions' publication after their segments changed.
func (s *Service) RepublishTx(ctx context.Context, tx pgx.Tx, versions []Target) error {
	for _, v := range versions {
		if err := s.enqueueTx(ctx, tx, publishArgs{Content: v.Content, Version: v.Version}); err != nil {
			return err
		}
	}
	return nil
}

// DeleteTx queues the deletion of targets' media, their publications'
// included, before their rows go.
func (s *Service) DeleteTx(ctx context.Context, tx pgx.Tx, targets []Target) error {
	refs, kinds, err := s.itemRefs(ctx, tx, targets)
	if err != nil {
		return err
	}
	del := make([]media.Deletion, len(refs))
	for i, ref := range refs {
		del[i] = media.Deletion{Ref: ref}
	}
	if err := s.m.Jobs.DeleteItemsTx(ctx, tx, del...); err != nil {
		return err
	}
	for kind, versions := range kinds {
		k, err := s.reg.Kind(kind)
		if err != nil || k.VersionKind() == nil {
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
		if err := s.retireTx(ctx, tx, k.VersionKind(), ids); err != nil {
			return err
		}
	}
	return nil
}

// itemRefs are targets' media items, and their versions by kind.
func (s *Service) itemRefs(ctx context.Context, q querier, targets []Target) ([]contentref.ContentRef, map[string][]string, error) {
	contents, versions := make([]string, len(targets)), make([]string, len(targets))
	for i, t := range targets {
		contents[i], versions[i] = t.Content, t.Version
	}
	rows, err := q.Query(ctx, `SELECT c.kind, c.id, t.version = '', COALESCE(v.id, '') FROM unnest($2::text[], $3::text[]) AS t(content, version)
JOIN `+s.t.items+` c ON c.tenant_id = $1 AND c.id = t.content
LEFT JOIN `+s.t.versions+` v ON v.tenant_id = $1 AND v.content_id = c.id AND (t.version = '' OR v.id = t.version)`, s.o.Tenant, contents, versions)
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
		var whole bool
		if err := rows.Scan(&kind, &id, &whole, &version); err != nil {
			return nil, nil, err
		}
		if whole {
			add(kind, id, "")
		}
		if version != "" {
			add(kind, id, version)
			byKind[kind] = append(byKind[kind], version)
		}
	}
	return refs, byKind, rows.Err()
}
