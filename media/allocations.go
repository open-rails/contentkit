package media

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// ErrAllocationRetired means a producer must write a new physical allocation
// rather than adopt an object already selected for cleanup.
var ErrAllocationRetired = errors.New("media: file allocation is missing or retired")

// TemporaryRetention is the maximum lifetime of an unfinished upload or
// optional cache. Permanent media must never depend on temp storage.
const TemporaryRetention = 7 * 24 * time.Hour
const minimumLateUploadWindow = TemporaryRetention + time.Hour

// ReserveTemporary accounts for an optional temporary object before its PUT.
// Reusing the same live key is safe; retired names can never be resurrected.
func (m *Manifests) ReserveTemporary(ctx context.Context, ref contentref.ContentRef, name string) error {
	if !layout.ValidSegment(name) {
		return errors.New("media: invalid temporary object name")
	}
	item, err := m.reg.Item(ref)
	if err != nil {
		return err
	}
	key := item.TempPrefix() + name
	return m.cleanup(ctx, item, false, func(cur *Manifest, exists bool) (journalEffects, error) {
		if !exists || cur.Deleted {
			return journalEffects{}, ErrNotFound
		}
		j := m.journal.forTenant(ref.TenantID)
		var present, live bool
		err := j.pool.QueryRow(ctx, `SELECT count(*) > 0, coalesce(bool_and(retired_at IS NULL AND incarnation = $4 AND created_at > now() - interval '7 days'),false) FROM `+j.allocations+` WHERE tenant_id=$1 AND folder_prefix=$2 AND object_key=$3`, ref.TenantID, item.Prefix(), key, cur.Incarnation).Scan(&present, &live)
		if err != nil {
			return journalEffects{}, err
		}
		if present {
			if !live {
				return journalEffects{}, ErrAllocationRetired
			}
			return journalEffects{}, nil
		}
		return journalEffects{Allocate: []string{key}, Incarnation: cur.Incarnation}, nil
	})
}

// NewBlobs records ownership, in one journal step, before a writer sends
// bytes to fresh allocations in area (private/ or download/, Kind.Area): one
// blob reference per digest, in order. The digest identifies the bytes; the
// UUID isolates their lifetime. This does not publish the objects: a later
// manifest edit must adopt them.
func (m *Manifests) NewBlobs(ctx context.Context, ref contentref.ContentRef, area string, sums [][]byte) ([]string, error) {
	item, err := m.reg.Item(ref)
	if err != nil {
		return nil, err
	}
	if item.Kind().Versions && (area == layout.AreaPrivate || area == layout.AreaDownload) {
		area = layout.AreaFuture
	}
	return m.newBlobs(ctx, ref, area, sums)
}

// newBlobs allocates an exact storage area; only explicit publication may use
// this to create served version objects.
func (m *Manifests) newBlobs(ctx context.Context, ref contentref.ContentRef, area string, sums [][]byte) ([]string, error) {
	item, err := m.reg.Item(ref)
	if err != nil {
		return nil, err
	}
	if area != layout.AreaPrivate && area != layout.AreaDownload && area != layout.AreaOriginals && area != layout.AreaFuture && area != layout.AreaPublic {
		return nil, fmt.Errorf("media: no blobs in area %q", area)
	}
	names, keys := make([]string, len(sums)), make([]string, len(sums))
	for i, sum := range sums {
		if len(sum) != sha256.Size {
			return nil, errors.New("media: blob digest must be SHA-256")
		}
		names[i] = layout.BlobRef(area, layout.BlobName(sum, uuid.NewString()))
		keys[i], _ = item.Blob(names[i])
	}
	if len(keys) == 0 {
		return names, nil
	}
	if err := m.allocate(ctx, item, keys); err != nil {
		return nil, err
	}
	return names, nil
}

func (m *Manifests) allocate(ctx context.Context, item Item, keys []string) error {
	return m.cleanup(ctx, item, false, func(cur *Manifest, exists bool) (journalEffects, error) {
		if !exists {
			// cleanup has recorded the scoped folder owner under the manifest
			// lock. Publish the empty root only after that receipt exists, so a
			// concurrent presign cannot observe an ownerless version manifest.
			var err error
			cur, err = m.ensureIncarnation(ctx, item)
			if err != nil {
				return journalEffects{}, err
			}
		}
		if cur.Deleted {
			return journalEffects{}, ErrNotFound
		}
		return journalEffects{Allocate: keys, Incarnation: cur.Incarnation}, nil
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

func manifestAllocations(item Item, m *Manifest) []string {
	var keys []string
	for _, name := range m.Blobs() {
		key, _ := item.Blob(name)
		keys = append(keys, key)
	}
	for _, name := range m.StagedNames() {
		key, _ := item.Staged(name)
		keys = append(keys, key)
	}
	return keys
}

func (m *Manifests) checkAllocationsIn(ctx context.Context, item Item, keys []string, incarnation string) error {
	journal := m.journal.forTenant(item.Ref().TenantID)
	if len(keys) == 0 {
		return nil
	}
	var live bool
	err := journal.pool.QueryRow(ctx, `SELECT NOT EXISTS (
SELECT 1 FROM unnest($3::text[]) AS requested(key)
WHERE NOT EXISTS (SELECT 1 FROM `+journal.allocations+` a
WHERE a.tenant_id = $1 AND a.folder_prefix = $2 AND a.object_key = requested.key AND a.incarnation = $4 AND a.retired_at IS NULL
AND (NOT starts_with(a.object_key, $5) OR a.created_at > now() - interval '7 days')))`,
		item.Ref().TenantID, item.Prefix(), keys, incarnation, item.TempPrefix()).Scan(&live)
	if err != nil {
		return err
	}
	if !live {
		return ErrAllocationRetired
	}
	return nil
}

func (m *Manifests) retireAllocations(ctx context.Context, item Item, keys []string) error {
	journal := m.journal.forTenant(item.Ref().TenantID)
	if len(keys) == 0 {
		return nil
	}
	_, err := journal.pool.Exec(ctx, `UPDATE `+journal.allocations+`
SET retired_at = now() WHERE tenant_id = $1 AND folder_prefix = $2 AND object_key = ANY($3::text[]) AND retired_at IS NULL`,
		item.Ref().TenantID, item.Prefix(), keys)
	return err
}

// allocationKeys includes writes that have not reached S3 yet. Immediate
// takedown retires those too; a late upload cannot revive their ownership.
func (m *Manifests) allocationKeys(ctx context.Context, item Item) ([]string, error) {
	journal := m.journal.forTenant(item.Ref().TenantID)
	rows, err := journal.pool.Query(ctx, `SELECT object_key FROM `+journal.allocations+`
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

// allocationDeadlines includes expired allocations: a cleaner must retire even
// an absent object and retain its exact key for the late-upload pass. Otherwise
// a producer could upload after the only cleanup obligation was acknowledged.
func (m *Manifests) allocationDeadlines(ctx context.Context, item Item, grace time.Duration) (map[string]time.Time, error) {
	journal := m.journal.forTenant(item.Ref().TenantID)
	rows, err := journal.pool.Query(ctx, `SELECT object_key, created_at FROM `+journal.allocations+`
WHERE tenant_id = $1 AND folder_prefix = $2 AND retired_at IS NULL`,
		item.Ref().TenantID, item.Prefix())
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
		retention := grace
		if strings.HasPrefix(key, item.TempPrefix()) {
			retention = TemporaryRetention
		}
		keys[key] = created.Add(retention)
	}
	return keys, rows.Err()
}
