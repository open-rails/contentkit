package media

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// ErrAllocationRetired means a producer must write a new physical allocation
// rather than adopt an object already selected for cleanup.
var ErrAllocationRetired = errors.New("media: file allocation is missing or retired")

// NewBlob records ownership before a worker sends bytes to a fresh private
// name. The digest identifies the bytes; the UUID isolates their lifetime.
// This does not publish the object: a later manifest edit must adopt it.
func (m *Manifests) NewBlob(ctx context.Context, ref contentref.ContentRef, sum []byte) (string, error) {
	item, err := m.reg.Item(ref)
	if err != nil {
		return "", err
	}
	if len(sum) != sha256.Size {
		return "", errors.New("media: blob digest must be SHA-256")
	}
	name := layout.BlobName(sum, uuid.NewString())
	key, _ := item.Blob(name)
	if err := m.allocate(ctx, item, key); err != nil {
		return "", err
	}
	return name, nil
}

func (m *Manifests) allocate(ctx context.Context, item Item, key string) error {
	if _, _, err := m.Get(ctx, item.Ref()); errors.Is(err, ErrNotFound) {
		// Establish the S3 incarnation before issuing a write capability.
		// Edit refuses a tombstone; only explicit reset can clear it.
		if _, err := m.Edit(ctx, item.Ref(), func(*Manifest) error { return nil }); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return m.cleanup(ctx, item, false, func(cur *Manifest, exists bool) (journalEffects, error) {
		if !exists || cur.Deleted {
			return journalEffects{}, ErrNotFound
		}
		return journalEffects{Allocate: []string{key}, Incarnation: cur.Incarnation}, nil
	})
}

// checkAllocations runs under the folder lock, after its journal lease was
// acquired. Recovery invalidates that lease before cleanup can retire keys.
func (m *Manifests) checkAllocations(ctx context.Context, item Item, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	cur, _, err := m.Get(ctx, item.Ref())
	if err != nil {
		return err
	}
	return m.checkAllocationsIn(ctx, item, keys, cur.Incarnation)
}

func (m *Manifests) checkAllocationsIn(ctx context.Context, item Item, keys []string, incarnation string) error {
	if len(keys) == 0 {
		return nil
	}
	var live bool
	err := m.journal.pool.QueryRow(ctx, `SELECT NOT EXISTS (
SELECT 1 FROM unnest($3::text[]) AS requested(key)
WHERE NOT EXISTS (SELECT 1 FROM `+m.journal.allocations+` a
WHERE a.tenant_id = $1 AND a.folder_prefix = $2 AND a.object_key = requested.key AND a.incarnation = $4 AND a.retired_at IS NULL))`,
		item.Ref().TenantID, item.Prefix(), keys, incarnation).Scan(&live)
	if err != nil {
		return err
	}
	if !live {
		return ErrAllocationRetired
	}
	return nil
}

func (m *Manifests) retireAllocations(ctx context.Context, item Item, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	_, err := m.journal.pool.Exec(ctx, `UPDATE `+m.journal.allocations+`
SET retired_at = now() WHERE tenant_id = $1 AND folder_prefix = $2 AND object_key = ANY($3::text[]) AND retired_at IS NULL`,
		item.Ref().TenantID, item.Prefix(), keys)
	return err
}

// allocationKeys includes writes that have not reached S3 yet. Immediate
// takedown retires those too; a late upload cannot revive their ownership.
func (m *Manifests) allocationKeys(ctx context.Context, item Item) ([]string, error) {
	rows, err := m.journal.pool.Query(ctx, `SELECT object_key FROM `+m.journal.allocations+`
WHERE tenant_id = $1 AND folder_prefix = $2`, item.Ref().TenantID, item.Prefix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// protectedAllocations gives an unrecorded producer its bounded grace period,
// including the interval before its object exists. Retired keys get no grace.
func (m *Manifests) protectedAllocations(ctx context.Context, item Item, now time.Time, grace time.Duration) (map[string]time.Time, error) {
	rows, err := m.journal.pool.Query(ctx, `SELECT object_key, created_at FROM `+m.journal.allocations+`
WHERE tenant_id = $1 AND folder_prefix = $2 AND retired_at IS NULL AND created_at > $3`,
		item.Ref().TenantID, item.Prefix(), now.Add(-grace))
	if err != nil {
		return nil, fmt.Errorf("media: allocation protection: %w", err)
	}
	defer rows.Close()
	keys := map[string]time.Time{}
	for rows.Next() {
		var key string
		var created time.Time
		if err := rows.Scan(&key, &created); err != nil {
			return nil, err
		}
		keys[key] = created.Add(grace)
	}
	return keys, rows.Err()
}
