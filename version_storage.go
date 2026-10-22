package contentkit

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/token"
)

// MigrateVersionStorage is the explicit object-store phase after
// MigratePostgres. It checkpoints verified copies in the existing media journal
// and schedules old nested folders for safe retirement. It never runs at
// startup. Call with old app/media writers stopped; the caller retains its pool
// and Store. No auth, billing or background worker runtime is constructed.
func MigrateVersionStorage(ctx context.Context, pool *pgxpool.Pool, cfg Config, store media.Store) (media.VersionStorageReport, error) {
	var report media.VersionStorageReport
	if cfg.Media == nil {
		return report, nil
	}
	e := &Engine{cfg: cfg, pool: pool}
	if err := e.configureStorage(); err != nil {
		return report, err
	}
	mc, err := e.cfg.MediaKinds()
	if err != nil {
		return report, err
	}
	versioned := false
	for _, kind := range mc.Kinds {
		versioned = versioned || kind.Versions
	}
	if !versioned {
		return report, nil
	}
	if pool == nil || store == nil {
		return report, errors.New("contentkit: version storage migration requires PostgreSQL and Store")
	}
	reg, journal, err := e.cleanupStorage()
	if err != nil {
		return report, err
	}
	ttl, window := e.cfg.Media.Delivery.TTL, e.cfg.Media.Delivery.Window
	if ttl <= 0 {
		ttl = time.Hour
	}
	if window <= 0 {
		window = token.DefaultWindow
	}
	jobs, err := media.NewJobs(media.JobsConfig{Store: store, Registry: reg, Journal: journal, Locker: media.PGLocker(pool), Grace: max(24*time.Hour, ttl+window+time.Second)})
	if err != nil {
		return report, err
	}
	return jobs.MigrateVersionStorage(ctx, e.riverSchema, media.VersionStorageOptions{VersionPurged: e.versionStoragePurged})
}

// Version IDs are immutable. Only a physically removed catalog row, never a
// restorable soft-deletion, can justify an already-empty historical folder.
func (e *Engine) versionStoragePurged(ctx context.Context, ref contentref.ContentRef) (bool, error) {
	if _, ok := e.cfg.Content.Kinds[ref.ContentKind]; !ok {
		return false, nil
	}
	var parent, kind string
	err := e.pool.QueryRow(ctx, `SELECT v.content_id,c.kind FROM `+pgx.Identifier{e.schema, "content_versions"}.Sanitize()+` v
LEFT JOIN `+pgx.Identifier{e.schema, "content_items"}.Sanitize()+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
WHERE v.tenant_id=$1 AND v.id=$2`, e.cfg.Tenant, ref.Version()).Scan(&parent, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if parent != ref.ContentID || kind != ref.ContentKind {
		return false, media.ErrCommitIdentity
	}
	return false, nil
}
