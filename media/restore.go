package media

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// ObjectVersion identifies immutable bytes in a versioned bucket.
type ObjectVersion struct {
	Key, VersionID string
}

// Snapshot is a read-only historical item, with a version for every reference.
// The versioned store creates it; hosts must authorize restore independently of
// historical visibility and restore their own records before applying it.
type Snapshot struct {
	Ref       contentref.ContentRef
	VersionID string
	Manifest  *Manifest
	Objects   []ObjectVersion
}

// VersionCopier is the historical-byte boundary used by Restore. Implemented by
// media/s3.Store; it must copy the exact version, never substitute current bytes.
type VersionCopier interface {
	CopyVersion(context.Context, ObjectVersion, string) (Object, error)
}

// RestoreOptions identifies the authorized restore and its quota owner.
type RestoreOptions struct {
	OperationID string // caller-stable UUID, reused unchanged after any failure
	Owner       string // required when JobsConfig.Limiter is configured
}

// Restore publishes a snapshot through the same journal as uploads. Every file
// receives a fresh allocation and the item a fresh incarnation, so old cleanup,
// queued erasure and delayed writes cannot affect the restored content. Public
// generations are never restored; current host visibility governs regeneration.
func (j *Jobs) Restore(ctx context.Context, snap Snapshot, o RestoreOptions) (*Manifest, error) {
	operation, err := uuid.Parse(o.OperationID)
	if err != nil || operation == uuid.Nil {
		return nil, errors.New("media: restore needs a caller-stable operation_id UUID")
	}
	item, err := j.cfg.Registry.Item(snap.Ref)
	if err != nil {
		return nil, err
	}
	if snap.VersionID == "" || snap.VersionID == "null" || snap.Manifest == nil || snap.Manifest.Deleted {
		return nil, errors.New("media: restore needs a readable, immutable live snapshot")
	}
	if err := snap.Manifest.Validate(); err != nil {
		return nil, err
	}
	versions := make(map[string]ObjectVersion, len(snap.Objects))
	for _, object := range snap.Objects {
		key, ok := layout.Parse(object.Key)
		if !ok || !strings.HasPrefix(object.Key, item.Prefix()) || (key.Area != layout.AreaPrivate && key.Area != layout.AreaTemp) ||
			object.VersionID == "" || object.VersionID == "null" {
			return nil, errors.New("media: snapshot contains an invalid object version")
		}
		if _, duplicate := versions[object.Key]; duplicate {
			return nil, errors.New("media: snapshot repeats an object version")
		}
		versions[object.Key] = object
	}
	var sources []string
	for _, name := range snap.Manifest.Blobs() {
		key, err := item.Blob(name)
		if err != nil {
			return nil, err
		}
		sources = append(sources, key)
	}
	for _, name := range snap.Manifest.StagedNames() {
		key, err := item.Staged(name)
		if err != nil {
			return nil, err
		}
		sources = append(sources, key)
	}
	slices.Sort(sources)
	sources = slices.Compact(sources)
	if len(sources) != len(versions) {
		return nil, errors.New("media: snapshot versions do not match its references")
	}
	for _, key := range sources {
		if _, ok := versions[key]; !ok {
			return nil, fmt.Errorf("media: snapshot lacks version for %s", key)
		}
	}
	input, err := json.Marshal(struct {
		Restore Snapshot
		Owner   string
	}{snap, o.Owner})
	if err != nil {
		return nil, err
	}
	mutation := &manifestMutation{lifecycle: true, commit: manifestCommit{ID: operation, Ref: snap.Ref,
		Folder: item.Prefix(), Fingerprint: sha256.Sum256(input)}}
	if current, found, err := j.manifests.replayCommit(ctx, item, mutation.commit); found || err != nil {
		return current, err
	}
	copier, ok := j.cfg.Store.(VersionCopier)
	if !ok {
		return nil, ErrNotImplemented
	}
	if j.cfg.Limiter != nil {
		if o.Owner == "" {
			return nil, errors.New("media: restore needs a quota owner")
		}
		mutation.quota, err = j.cfg.Limiter.Quota(ctx, snap.Ref.TenantID, o.Owner)
		if err != nil {
			return nil, err
		}
	}
	out, err := j.manifests.editOperation(ctx, snap.Ref, false, bound{}, mutation, func(cur *Manifest) error {
		next := snap.Manifest.Clone()
		next.Incarnation, next.Deleted, next.Full, next.Deficit = uuid.NewString(), false, false, 0
		next.Receipt = nil
		next.Hidden = true
		if j.cfg.Registry.cfg.Hooks.Resolver != nil {
			hidden, err := j.hidden(ctx, snap.Ref)
			if err != nil {
				return err
			}
			next.Hidden = hidden
		}
		// Select old targets before reserving the new ones. Include allocations
		// whose writes have not arrived, not just objects already in the bucket.
		keys, err := j.manifests.allocationKeys(ctx, item)
		if err != nil {
			return err
		}
		mutation.effects = journalEffects{Deletion: true, Private: keys, Cancel: j.cfg.Journal.queue != nil,
			Notify: j.cfg.Registry.cfg.Hooks.ItemCommitted != nil}
		for obj, err := range j.cfg.Store.List(ctx, item.Prefix()) {
			if err != nil {
				return err
			}
			if strings.HasPrefix(obj.Key, item.PublicPrefix()) {
				mutation.effects.Public = append(mutation.effects.Public, obj.Key)
			} else if obj.Key != item.ManifestKey() && !slices.Contains(keys, obj.Key) {
				mutation.effects.Private = append(mutation.effects.Private, obj.Key)
			}
		}
		for _, f := range cur.Files {
			for _, p := range f.Public {
				for _, name := range p.NamesOnDisk() {
					key, _ := item.Public(name)
					if !slices.Contains(mutation.effects.Public, key) {
						mutation.effects.Public = append(mutation.effects.Public, key)
					}
				}
			}
		}
		remap := make(map[string]string, len(sources))
		destinations := make([]string, len(sources))
		for i, key := range sources {
			parsed, _ := layout.Parse(key)
			name := layout.StagedPrefix + uuid.NewString()
			if parsed.Area == layout.AreaPrivate {
				digest, _ := layout.BlobDigest(parsed.Name)
				name = layout.BlobName(digest, uuid.NewString())
			}
			remap[parsed.Name] = name
			destinations[i] = item.Prefix() + parsed.Area + "/" + name
		}
		if err := j.cfg.Journal.allocateRestore(ctx, mutation.commit, next.Incarnation, destinations); err != nil {
			return err
		}
		for i, key := range sources {
			if _, err := copier.CopyVersion(ctx, versions[key], destinations[i]); err != nil {
				return err
			}
		}
		for i := range next.Files {
			f := &next.Files[i]
			oldKey := f.Key()
			if name, ok := remap[f.Blob]; ok {
				f.Blob = name
			}
			if name, ok := remap[f.Staged]; ok {
				f.Staged = name
			}
			if f.Editor != nil {
				if name, ok := remap[f.Editor.Blob]; ok {
					f.Editor.Blob = name
				}
			}
			if f.Track != nil && f.Track.Index != "" {
				f.Track.Index = remap[f.Track.Index]
			}
			if f.Frame != nil {
				if name, ok := remap[f.Frame.Of]; ok {
					f.Frame.Of = name
				}
			}
			if f.Failed != nil && f.Failed.Of == oldKey {
				f.Failed.Of = f.Key()
			}
			f.Public = nil
			if f.IsUpload() && !f.Gone && f.Fail() == nil {
				f.Pending = item.Kind().Presets(f.Path, next.Hidden)
			}
		}
		if j.cfg.Limiter != nil {
			before, err := j.uploadBytes(ctx, item.Prefix())
			if err != nil {
				return err
			}
			mutation.effects.Settlement = Settlement{Tenant: snap.Ref.TenantID, Owner: o.Owner,
				Delta: next.uploadBytes() - before, Enforce: true}
		}
		if j.cfg.Journal.queue != nil {
			mutation.effects.Process = &ProcessJob{Ref: snap.Ref, Place: len(next.StagedNames()) > 0}
		}
		*cur = *next
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, j.manifests.notifyCommit(ctx, mutation.commit)
}

// Ownership precedes historical copies. Recovery may invalidate this open lease
// while bytes are in flight; final prepare then refuses to publish their root.
func (j *PGJournal) allocateRestore(ctx context.Context, c manifestCommit, incarnation string, keys []string) error {
	return pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error {
		found, err := j.load(ctx, tx, c.Ref.TenantID, c.ID)
		if err != nil {
			return err
		}
		if found.Lease != c.Lease || found.State != "open" {
			return ErrCommitPending
		}
		for _, key := range keys {
			if _, err := tx.Exec(ctx, `INSERT INTO `+j.allocations+`
(tenant_id, folder_prefix, object_key, incarnation) VALUES ($1, $2, $3, $4)`,
				c.Ref.TenantID, c.Folder, key, incarnation); err != nil {
				return err
			}
		}
		return nil
	})
}
