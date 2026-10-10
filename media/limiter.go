package media

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UploadLimiter is the optional anti-abuse port. Quota is enforced when bytes
// are committed: a commit that grows its owner's stored originals past the
// quota is refused. Reserve runs at presign (skipped for exempt uploaders) and
// refuses with an *UploadError coded CodeRate or CodeQuota before any bytes
// move; a reservation is only that early refusal and may expire. Settle runs
// at commit and abort: it drops the reservations of Keys and adds Delta (the
// change in stored originals, negative on removal) to the owner's usage.
type UploadLimiter interface {
	Reserve(ctx context.Context, r Reservation) error
	Settle(ctx context.Context, s Settlement) error
}

// QuotaReleaser records a folder's refund before its objects are deleted, then
// applies that refund once. Operation identifies the deletion across retries.
type QuotaReleaser interface {
	PrepareRelease(ctx context.Context, r QuotaRelease) (applied bool, err error)
	Release(ctx context.Context, tenant, operation string) error
}

// QuotaRelease is the amount captured before deleting a folder.
type QuotaRelease struct {
	Tenant    string
	Folder    string
	Owner     string
	Operation string
	Bytes     int64
}

// ErrQuotaReleasePending means a different deletion must finish or be retried first.
var ErrQuotaReleasePending = errors.New("media: another quota release is pending for the folder")

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
	// Quota is the owner's storage cap in bytes (<= 0 unlimited); nil disables quotas.
	Quota func(ctx context.Context, tenant, owner string) (int64, error)
	// ReservationTTL drops unsettled reservations (commit still enforces the
	// quota); default 24h.
	ReservationTTL time.Duration
}

// PGLimiter is the default UploadLimiter over ContentKit's Postgres schema:
// hourly counters per uploader (bytes/day sums the last 24), one usage total
// per owner and short-lived pending reservations. There are no rows per file.
type PGLimiter struct {
	pool     *pgxpool.Pool
	limits   PGLimits
	rates    string
	usage    string
	res      string
	releases string
}

var _ UploadLimiter = (*PGLimiter)(nil)
var _ QuotaReleaser = (*PGLimiter)(nil)

func NewPGLimiter(pool *pgxpool.Pool, schema string, limits PGLimits) (*PGLimiter, error) {
	if pool == nil || schema == "" {
		return nil, errors.New("media: PGLimiter needs a pool and the ContentKit schema")
	}
	if limits.ReservationTTL <= 0 {
		limits.ReservationTTL = 24 * time.Hour
	}
	q := func(t string) string { return pgx.Identifier{schema, t}.Sanitize() }
	return &PGLimiter{pool: pool, limits: limits, rates: q("content_media_upload_rates"),
		usage: q("content_media_usage"), res: q("content_media_reservations"),
		releases: q("content_media_releases")}, nil
}

func (l *PGLimiter) PrepareRelease(ctx context.Context, r QuotaRelease) (bool, error) {
	if r.Tenant == "" || r.Folder == "" || r.Owner == "" || r.Operation == "" || r.Bytes < 0 {
		return false, errors.New("media: release needs a tenant, folder, owner, operation and nonnegative bytes")
	}
	var applied bool
	err := pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO `+l.releases+` (tenant_id, operation_id, folder_prefix, owner_id, bytes)
VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`,
			r.Tenant, r.Operation, r.Folder, r.Owner, r.Bytes); err != nil {
			return err
		}
		var folder string
		var owner string
		var bytes int64
		if err := tx.QueryRow(ctx, `SELECT folder_prefix, owner_id, bytes, applied FROM `+l.releases+`
WHERE tenant_id = $1 AND operation_id = $2`, r.Tenant, r.Operation).Scan(&folder, &owner, &bytes, &applied); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: %s", ErrQuotaReleasePending, r.Folder)
			}
			return err
		}
		if folder != r.Folder || owner != r.Owner || !applied && r.Bytes > bytes {
			return fmt.Errorf("media: release operation %q changed folder, owner or grew", r.Operation)
		}
		return nil
	})
	return applied, err
}

func (l *PGLimiter) Release(ctx context.Context, tenant, operation string) error {
	return pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		var owner string
		var bytes int64
		var applied bool
		if err := tx.QueryRow(ctx, `SELECT owner_id, bytes, applied FROM `+l.releases+`
WHERE tenant_id = $1 AND operation_id = $2 FOR UPDATE`, tenant, operation).Scan(&owner, &bytes, &applied); err != nil {
			return fmt.Errorf("media: load release %q: %w", operation, err)
		}
		if applied {
			return nil
		}
		if bytes > 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO `+l.usage+` AS u (tenant_id, owner_id, used_bytes) VALUES ($1, $2, 0)
ON CONFLICT (tenant_id, owner_id) DO UPDATE SET used_bytes = GREATEST(u.used_bytes - $3, 0)`,
				tenant, owner, bytes); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE `+l.releases+` SET applied = true
WHERE tenant_id = $1 AND operation_id = $2`, tenant, operation)
		return err
	})
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
			return &UploadError{Code: CodeQuota, Message: fmt.Sprintf("storage quota exceeded: %d used, %d more committed of %d", used, s.Delta, quota)}
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

// Usage reports an owner's stored bytes and unexpired pending reservations.
func (l *PGLimiter) Usage(ctx context.Context, tenant, owner string) (used, pending int64, err error) {
	err = l.pool.QueryRow(ctx, `SELECT COALESCE((SELECT used_bytes FROM `+l.usage+` WHERE tenant_id = $1 AND owner_id = $2), 0),
(SELECT COALESCE(sum(bytes), 0) FROM `+l.res+` WHERE tenant_id = $1 AND owner_id = $2 AND expires_at > now())`,
		tenant, owner).Scan(&used, &pending)
	return used, pending, err
}
