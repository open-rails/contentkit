package mediastore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	media "github.com/open-rails/contentkit/media"
)

// Reservation is one presigned upload. Uploader is rate-limited; Owner ("" for
// none) has Size reserved against its quota until the upload is settled or the
// reservation expires.
type Reservation struct {
	Tenant   string
	Uploader string
	Owner    string
	Key      string
	Size     int64
}

// Settlement releases reservations and moves an owner's usage. With Enforce,
// a positive Delta that takes usage over the owner's quota is refused with
// CodeQuota and changes nothing.
type Settlement struct {
	Tenant  string
	Owner   string
	Keys    []string
	Delta   int64
	Enforce bool
}

// PGLimits configure PGLimiter. Zero limits are off.
type PGLimits struct {
	FilesPerHour int
	BytesPerDay  int64
	// Rates, when set, are an upload's per-uploader limits by the quota
	// owner it writes for, in place of FilesPerHour and BytesPerDay.
	Rates func(ctx context.Context, tenant, owner string) (filesPerHour int, bytesPerDay int64, err error)
	// Quota is the owner's storage cap in bytes (<= 0 unlimited); nil disables quotas.
	Quota func(ctx context.Context, tenant, owner string) (int64, error)
	// ReservationTTL drops unsettled reservations (commit still enforces the
	// quota); default 24h.
	ReservationTTL time.Duration
}

// PGLimiter is the upload anti-abuse limiter over ContentKit's Postgres
// schema: hourly counters per uploader (bytes/day sums the last 24), one usage
// total per owner and short-lived pending reservations. There are no rows per
// file. Quota is enforced when bytes are committed: a commit that grows its
// owner's stored originals past the quota is refused. Reserve runs at presign
// (skipped for exempt uploaders) and refuses with an *UploadError coded
// CodeRate or CodeQuota before any bytes move; a reservation is only that
// early refusal and may expire. Settle runs at commit and abort: it drops the
// reservations of Keys and adds Delta (the change in stored originals,
// negative on removal) to the owner's usage.
type PGLimiter struct {
	pool   *pgxpool.Pool
	limits PGLimits
	rates  string
	usage  string
	res    string
	routes map[string]*PGLimiter
}

func NewPGLimiter(pool *pgxpool.Pool, schema string, limits PGLimits) (*PGLimiter, error) {
	if pool == nil || schema == "" {
		return nil, errors.New("media: PGLimiter needs a pool and the ContentKit schema")
	}
	if limits.ReservationTTL <= 0 {
		limits.ReservationTTL = 24 * time.Hour
	}
	q := func(t string) string { return pgx.Identifier{schema, t}.Sanitize() }
	return &PGLimiter{pool: pool, limits: limits, rates: q("content_media_upload_rates"),
		usage: q("content_media_usage"), res: q("content_media_reservations")}, nil
}

func (l *PGLimiter) Reserve(ctx context.Context, r Reservation) error {
	l = l.forTenant(r.Tenant)
	var quota int64
	if l.limits.Quota != nil && r.Owner != "" {
		var err error
		if quota, err = l.limits.Quota(ctx, r.Tenant, r.Owner); err != nil {
			return fmt.Errorf("media: quota for %s: %w", r.Owner, err)
		}
	}
	return pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		if err := l.rate(ctx, tx, r); err != nil {
			return err
		}
		if r.Owner == "" {
			return nil
		}
		// The usage row lock serializes one owner's reservations.
		var used, pending int64
		if err := tx.QueryRow(ctx, `INSERT INTO `+l.usage+` AS u (tenant_id, owner_id) VALUES ($1, $2)
ON CONFLICT (tenant_id, owner_id) DO UPDATE SET used_bytes = u.used_bytes RETURNING used_bytes`, r.Tenant, r.Owner).Scan(&used); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM `+l.res+` WHERE tenant_id = $1 AND owner_id = $2 AND expires_at <= now()`, r.Tenant, r.Owner); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(bytes), 0) FROM `+l.res+`
WHERE tenant_id = $1 AND owner_id = $2 AND object_key <> $3`, r.Tenant, r.Owner, r.Key).Scan(&pending); err != nil {
			return err
		}
		if quota > 0 && used+pending+r.Size > quota {
			return &media.UploadError{Code: media.CodeQuota, Message: fmt.Sprintf("storage quota exceeded: %d used, %d pending, %d requested of %d", used, pending, r.Size, quota)}
		}
		_, err := tx.Exec(ctx, `INSERT INTO `+l.res+` (tenant_id, object_key, owner_id, bytes, expires_at)
VALUES ($1, $2, $3, $4, now() + $5::interval)
ON CONFLICT (tenant_id, object_key) DO UPDATE SET owner_id = EXCLUDED.owner_id, bytes = EXCLUDED.bytes, expires_at = EXCLUDED.expires_at`,
			r.Tenant, r.Key, r.Owner, r.Size, l.limits.ReservationTTL)
		return err
	})
}

// rate counts the upload in the uploader's current hour and checks files in
// that hour and bytes over the last 24. The hour row lock serializes one
// uploader's presigns.
func (l *PGLimiter) rate(ctx context.Context, tx pgx.Tx, r Reservation) error {
	filesPerHour, bytesPerDay := l.limits.FilesPerHour, l.limits.BytesPerDay
	if l.limits.Rates != nil {
		var err error
		if filesPerHour, bytesPerDay, err = l.limits.Rates(ctx, r.Tenant, r.Owner); err != nil {
			return fmt.Errorf("media: upload rates for %s: %w", r.Owner, err)
		}
	}
	if filesPerHour <= 0 && bytesPerDay <= 0 {
		return nil
	}
	var files int
	var hour time.Time
	if err := tx.QueryRow(ctx, `INSERT INTO `+l.rates+` AS r (tenant_id, uploader, hour, files, bytes)
VALUES ($1, $2, date_trunc('hour', now()), 1, $3)
ON CONFLICT (tenant_id, uploader, hour) DO UPDATE SET files = r.files + 1, bytes = r.bytes + EXCLUDED.bytes
RETURNING files, hour`, r.Tenant, r.Uploader, r.Size).Scan(&files, &hour); err != nil {
		return err
	}
	if filesPerHour > 0 && files > filesPerHour {
		return &media.UploadError{Code: media.CodeRate, Message: fmt.Sprintf("upload rate limit: %d files per hour", filesPerHour),
			RetryAfter: time.Until(hour.Add(time.Hour))}
	}
	if bytesPerDay <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM `+l.rates+` WHERE tenant_id = $1 AND uploader = $2 AND hour <= now() - interval '1 day'`,
		r.Tenant, r.Uploader); err != nil {
		return err
	}
	var day int64
	var oldest time.Time
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(bytes), 0), COALESCE(min(hour), now()) FROM `+l.rates+`
WHERE tenant_id = $1 AND uploader = $2`, r.Tenant, r.Uploader).Scan(&day, &oldest); err != nil {
		return err
	}
	if day > bytesPerDay {
		return &media.UploadError{Code: media.CodeRate, Message: fmt.Sprintf("upload rate limit: %d bytes per day", bytesPerDay),
			RetryAfter: time.Until(oldest.Add(24 * time.Hour))}
	}
	return nil
}

func (l *PGLimiter) Settle(ctx context.Context, s Settlement) error {
	l = l.forTenant(s.Tenant)
	var quota int64
	if s.Enforce && s.Delta > 0 {
		var err error
		if quota, err = l.Quota(ctx, s.Tenant, s.Owner); err != nil {
			return err
		}
	}
	return pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		return l.SettleTx(ctx, tx, s, quota)
	})
}

// Quota resolves the owner's configured storage cap (<= 0 is unlimited).
// Call it before borrowing a transaction: the host callback may use the same
// pool. It does not read usage or reserve bytes.
func (l *PGLimiter) Quota(ctx context.Context, tenant, owner string) (int64, error) {
	if owner == "" || l.limits.Quota == nil {
		return 0, nil
	}
	quota, err := l.limits.Quota(ctx, tenant, owner)
	if err != nil {
		return 0, fmt.Errorf("media: quota for %s: %w", owner, err)
	}
	return quota, nil
}

// SettleTx applies a settlement in the caller's transaction, so a recovery
// receipt and queue effects can commit or roll back with it. quota is the cap
// resolved by Quota before starting tx; it is checked only for enforced growth.
// The caller must roll back tx on error. This method does not start another
// transaction, call host code, or make the settlement independently idempotent.
func (l *PGLimiter) SettleTx(ctx context.Context, tx pgx.Tx, s Settlement, quota int64) error {
	if s.Owner != "" && (s.Delta != 0 || len(s.Keys) > 0) {
		// Reserve takes this lock before touching reservations. Keep that order
		// for refunds and unenforced recovery settlements as well.
		var used int64
		if err := tx.QueryRow(ctx, `INSERT INTO `+l.usage+` AS u (tenant_id, owner_id) VALUES ($1, $2)
ON CONFLICT (tenant_id, owner_id) DO UPDATE SET used_bytes = u.used_bytes RETURNING used_bytes`, s.Tenant, s.Owner).Scan(&used); err != nil {
			return err
		}
		if s.Enforce && s.Delta > 0 && quota > 0 && used+s.Delta > quota {
			return &media.UploadError{Code: media.CodeQuota, Message: fmt.Sprintf("storage quota exceeded: %d used, %d more committed of %d", used, s.Delta, quota)}
		}
	}
	if len(s.Keys) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM `+l.res+` WHERE tenant_id = $1 AND object_key = ANY($2)`, s.Tenant, s.Keys); err != nil {
			return err
		}
	}
	if s.Owner == "" || s.Delta == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE `+l.usage+` SET used_bytes = GREATEST(used_bytes + $3, 0)
WHERE tenant_id = $1 AND owner_id = $2`, s.Tenant, s.Owner, s.Delta)
	return err
}

// RouteLimiter pairs a routed journal with its account quota ledger.
func RouteLimiter(app, account *PGLimiter, partition string) *PGLimiter {
	if app == nil {
		return nil
	}
	copy := *app
	copy.routes = map[string]*PGLimiter{partition: account}
	return &copy
}

func (l *PGLimiter) forTenant(tenant string) *PGLimiter {
	if next := l.routes[tenant]; next != nil {
		return next
	}
	return l
}
