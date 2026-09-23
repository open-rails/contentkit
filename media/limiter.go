package media

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UploadLimiter is the optional anti-abuse port. Reserve runs at presign
// (skipped for exempt uploaders) and refuses with an *UploadError coded
// CodeRate or CodeQuota before any bytes move. Settle runs at commit, abort
// and item deletion: it drops the reservations of Keys and adds Delta (the
// change in stored originals, negative on removal) to the owner's usage.
type UploadLimiter interface {
	Reserve(ctx context.Context, r Reservation) error
	Settle(ctx context.Context, s Settlement) error
}

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

// Settlement releases reservations and moves an owner's usage.
type Settlement struct {
	Tenant string
	Owner  string
	Keys   []string
	Delta  int64
}

// PGLimits configure PGLimiter. Zero limits are off.
type PGLimits struct {
	FilesPerHour int
	BytesPerDay  int64
	// Quota is the owner's storage cap in bytes (<= 0 unlimited); nil disables quotas.
	Quota func(ctx context.Context, tenant, owner string) (int64, error)
	// ReservationTTL drops unsettled reservations; default 24h.
	ReservationTTL time.Duration
}

// PGLimiter is the default UploadLimiter over ContentKit's Postgres schema:
// hourly counters per uploader (bytes/day sums the last 24), one usage total
// per owner and short-lived pending reservations. There are no rows per file.
type PGLimiter struct {
	pool   *pgxpool.Pool
	limits PGLimits
	rates  string
	usage  string
	res    string
}

var _ UploadLimiter = (*PGLimiter)(nil)

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
			return &UploadError{Code: CodeQuota, Message: fmt.Sprintf("storage quota exceeded: %d used, %d pending, %d requested of %d", used, pending, r.Size, quota)}
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
	if l.limits.FilesPerHour <= 0 && l.limits.BytesPerDay <= 0 {
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
	if l.limits.FilesPerHour > 0 && files > l.limits.FilesPerHour {
		return &UploadError{Code: CodeRate, Message: fmt.Sprintf("upload rate limit: %d files per hour", l.limits.FilesPerHour),
			RetryAfter: time.Until(hour.Add(time.Hour))}
	}
	if l.limits.BytesPerDay <= 0 {
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
	if day > l.limits.BytesPerDay {
		return &UploadError{Code: CodeRate, Message: fmt.Sprintf("upload rate limit: %d bytes per day", l.limits.BytesPerDay),
			RetryAfter: time.Until(oldest.Add(24 * time.Hour))}
	}
	return nil
}

func (l *PGLimiter) Settle(ctx context.Context, s Settlement) error {
	return pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		if len(s.Keys) > 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM `+l.res+` WHERE tenant_id = $1 AND object_key = ANY($2)`, s.Tenant, s.Keys); err != nil {
				return err
			}
		}
		if s.Owner == "" || s.Delta == 0 {
			return nil
		}
		_, err := tx.Exec(ctx, `INSERT INTO `+l.usage+` AS u (tenant_id, owner_id, used_bytes) VALUES ($1, $2, GREATEST($3, 0))
ON CONFLICT (tenant_id, owner_id) DO UPDATE SET used_bytes = GREATEST(u.used_bytes + $3, 0)`, s.Tenant, s.Owner, s.Delta)
		return err
	})
}

// Usage reports an owner's stored bytes and unexpired pending reservations.
func (l *PGLimiter) Usage(ctx context.Context, tenant, owner string) (used, pending int64, err error) {
	err = l.pool.QueryRow(ctx, `SELECT COALESCE((SELECT used_bytes FROM `+l.usage+` WHERE tenant_id = $1 AND owner_id = $2), 0),
(SELECT COALESCE(sum(bytes), 0) FROM `+l.res+` WHERE tenant_id = $1 AND owner_id = $2 AND expires_at > now())`,
		tenant, owner).Scan(&used, &pending)
	return used, pending, err
}
