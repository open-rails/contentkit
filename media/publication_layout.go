package media

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/media/layout"
)

var ErrPublicationLayoutRequired = errors.New("media: explicit publication layout migration required")

// publicationLayoutPlan is an explicit migration receipt in the existing
// journal. Startup never creates it or discovers objects to populate it.
type publicationLayoutPlan struct {
	SourceDigest string         `json:"source_digest"`
	Manifest     *Manifest      `json:"manifest"`
	Copies       []DeliveryFile `json:"copies"`
	Retired      []string       `json:"retired"`
	Adopted      bool           `json:"adopted"`
	Complete     bool           `json:"complete"`
}

// PublicationLayoutReady is a local manifest check for the reader and
// publisher; it performs no migration or storage effects.
func (m *Manifest) PublicationLayoutReady() bool {
	return m != nil && m.PublicationLayout == PublicationLayoutVersion && !m.Deleted
}

// ValidatePublicationLayoutMigration is a read-only cold-start check over
// journal-owned version folders. It never scans a bucket, repairs a manifest
// or starts conversion. Engines and workers call it before accepting writes.
func (m *Manifests) ValidatePublicationLayoutMigration(ctx context.Context) error {
	for _, kind := range m.reg.Kinds() {
		if !kind.Versions {
			continue
		}
		journal := m.journal.forTenant(kind.ns)
		rows, err := journal.pool.Query(ctx, `SELECT DISTINCT content_id,content_version_id,folder_prefix FROM `+journal.table+`
WHERE tenant_id=$1 AND content_kind=$2 AND content_version_id IS NOT NULL ORDER BY folder_prefix`, kind.ns, kind.Name)
		if err != nil {
			return err
		}
		type owner struct{ Content, Version, Folder string }
		owners, err := pgx.CollectRows(rows, pgx.RowToStructByPos[owner])
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, entry := range owners {
			ref, err := m.reg.VersionRef(kind.Name, entry.Content, entry.Version)
			if err != nil {
				return err
			}
			item, err := m.reg.Item(ref)
			if err != nil {
				return err
			}
			if entry.Folder != item.Prefix() {
				// Prior nested storage has its own explicit owner/migration gate.
				// It cannot substitute for checking the canonical flat owner.
				if entry.Folder != kind.nestedPrefix(entry.Content, entry.Version) {
					return ErrCommitIdentity
				}
			}
			if seen[item.Prefix()] {
				continue
			}
			seen[item.Prefix()] = true
			if err := journal.checkFolderOwner(ctx, journal.pool, item, false); err != nil {
				return err
			}
			manifest, _, err := m.get(ctx, item.ManifestKey())
			if errors.Is(err, ErrNotFound) {
				var allocated, committed, erased bool
				err = journal.pool.QueryRow(ctx, `SELECT
EXISTS(SELECT 1 FROM `+journal.allocations+` WHERE tenant_id=$1 AND folder_prefix=$2),
EXISTS(SELECT 1 FROM `+journal.table+` WHERE tenant_id=$1 AND folder_prefix=$2 AND
 (state IN ('prepared','frozen') OR state='applied' AND effects->>'cleanup' IS DISTINCT FROM 'true')),
EXISTS(SELECT 1 FROM `+journal.table+` WHERE tenant_id=$1 AND folder_prefix=$2 AND state='applied' AND effects->>'erasure'='true')`, kind.ns, item.Prefix()).Scan(&allocated, &committed, &erased)
				if err != nil {
					return err
				}
				if !allocated && !committed && entry.Folder == item.Prefix() {
					continue
				}
				if erased {
					if _, err := m.store.Head(ctx, item.PublicPrefix()+layout.ErasureFenceName); err == nil {
						continue
					} else if !errors.Is(err, ErrNotFound) {
						return err
					}
				}
				return fmt.Errorf("%w: known version manifest is missing: %s", ErrPublicationLayoutRequired, item.Prefix())
			}
			if err != nil {
				return err
			}
			if manifest.Deleted {
				continue
			}
			if !manifest.PublicationLayoutReady() {
				return fmt.Errorf("%w: %s", ErrPublicationLayoutRequired, item.Prefix())
			}
		}
	}
	return nil
}
