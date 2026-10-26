package contentkit

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

// PublicationStorageReport is an explicit stopped-writer layout conversion.
// Queued editions still require their normal publication worker to complete
// before readers are switched to the new deployment.
type PublicationStorageReport struct {
	Staged int
	Queued int
}

// MigratePublicationStorage retains original source bytes, stages existing
// reader outputs in future, and queues each intended publication durably.
// Call after MigratePostgres and MigrateVersionStorage with old hosts/workers
// stopped. It never runs at startup and never discovers unrecorded S3 objects.
// Resolve legacy parent-wide billing entitlements to exact edition grants
// before enabling the new readers; this function does not mutate billing.
func MigratePublicationStorage(ctx context.Context, pool *pgxpool.Pool, cfg Config, store media.Store) (PublicationStorageReport, error) {
	var report PublicationStorageReport
	if cfg.Media == nil {
		return report, nil
	}
	if pool == nil || store == nil {
		return report, errors.New("contentkit: publication migration requires PostgreSQL and Store")
	}
	e := &Engine{cfg: cfg, pool: pool}
	if err := e.configureStorage(); err != nil {
		return report, err
	}
	registry, journal, err := e.cleanupStorage()
	if err != nil {
		return report, err
	}
	manifests, err := media.NewManifests(store, registry, media.ManifestOptions{Locker: media.PGLocker(pool), Journal: journal})
	if err != nil {
		return report, err
	}
	wake, err := versions.ProcessingPublisher(pool, e.riverSchema, e.schema, registry)
	if err != nil {
		return report, err
	}
	items := pgx.Identifier{e.schema, "content_items"}.Sanitize()
	editions := pgx.Identifier{e.schema, "content_versions"}.Sanitize()
	facets := pgx.Identifier{e.schema, "content_facets"}.Sanitize()
	for after := ""; ; {
		rows, err := pool.Query(ctx, `SELECT v.id,v.content_id,c.kind FROM `+editions+` v JOIN `+items+` c
 ON c.tenant_id=v.tenant_id AND c.id=v.content_id WHERE v.tenant_id=$1 AND v.id>$2 ORDER BY v.id LIMIT 100`, e.cfg.Tenant, after)
		if err != nil {
			return report, err
		}
		type edition struct{ ID, Content, Kind string }
		batch, err := pgx.CollectRows(rows, pgx.RowToStructByPos[edition])
		if err != nil {
			return report, err
		}
		for _, version := range batch {
			after = version.ID
			kind, err := registry.Kind(version.Kind)
			if err != nil || !kind.Versions {
				continue
			}
			ref, err := registry.VersionRef(version.Kind, version.Content, version.ID)
			if err != nil {
				return report, err
			}
			operation := uuid.NewSHA1(uuid.NameSpaceURL, []byte("contentkit-publication-layout:"+ref.String())).String()
			m, _, err := manifests.Get(ctx, ref)
			if errors.Is(err, media.ErrNotFound) {
				// The layout migration creates a media-less version's manifest.
				if err = manifests.MigratePublicationLayout(ctx, ref, operation); err == nil {
					m, _, err = manifests.Get(ctx, ref)
				}
			}
			if err != nil {
				return report, fmt.Errorf("publication layout %s: %w", version.ID, err)
			}
			if !m.Deleted && !m.PublicationLayoutReady() {
				if err := manifests.MigratePublicationLayout(ctx, ref, operation); err != nil {
					return report, fmt.Errorf("publication layout %s: %w", version.ID, err)
				}
				report.Staged++
			}
			if m.Deleted {
				continue
			}
			err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				// Completed new-layout publication is never reset by a repeated
				// migration. A failed enqueue remains eligible on the next call.
				var needs bool
				if err := tx.QueryRow(ctx, `SELECT release_id IS NULL FROM `+editions+`
 WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, e.cfg.Tenant, version.ID).Scan(&needs); err != nil || !needs {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE `+editions+` SET released=false WHERE tenant_id=$1 AND id=$2`, e.cfg.Tenant, version.ID); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE `+facets+` SET live_from=NULL WHERE tenant_id=$1 AND version_id=$2`, e.cfg.Tenant, version.ID); err != nil {
					return err
				}
				if err := wake(ctx, tx, ref); err != nil {
					return err
				}
				report.Queued++
				return nil
			})
			if err != nil {
				return report, err
			}
		}
		if len(batch) < 100 {
			// The metadata job runs only after startup. Admit the release request
			// now so the explicit handoff is complete before startup validates it.
			return report, e.migrateUnifiedReleases(ctx)
		}
	}
}
