package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/media/layout"
)

// deletionIncarnation captures even an item with no uploads without opening
// a second transaction while DeleteItemsTx holds the host's connection. An
// empty root has no coupled quota, queue or cleanup effects. Its conditional
// PUT cannot overwrite a tombstone or a concurrently initialized lifetime.
func (m *Manifests) deletionIncarnation(ctx context.Context, item Item) (*Manifest, error) {
	if err := RequireConditionalPut(ctx, m.store); err != nil {
		return nil, err
	}
	for range m.retries {
		cur, _, err := m.get(ctx, item.ManifestKey())
		if !errors.Is(err, ErrNotFound) {
			return cur, err
		}
		if err := m.requireFresh(ctx, item); err != nil {
			return nil, err
		}
		cur = &Manifest{V: ManifestVersion, Incarnation: uuid.NewString(), Hidden: true, Files: []File{}}
		body, err := encodeManifest(cur)
		if err != nil {
			return nil, err
		}
		obj, err := m.store.Put(ctx, item.ManifestKey(), bytes.NewReader(body), int64(len(body)), PutOptions{
			IfNoneMatch: "*", ContentType: "application/gzip", CacheControl: "no-store", Metadata: map[string]string{uploadBytesMeta: "0"}})
		if errors.Is(err, ErrPreconditionFailed) {
			continue
		} else if err != nil {
			return nil, err
		}
		m.cache.put(item.ManifestKey(), obj.ETag, cur)
		return cur, nil
	}
	return nil, ErrManifestConflict
}

// deleteItem keeps a tombstone at the current S3 key. Every side effect is
// bound to this operation's exact targets, including allocations whose PUT
// has not arrived. expected distinguishes a queued deletion from a reset.
var errOrphanChanged = errors.New("media: orphan folder changed during deletion")

func (j *Jobs) deleteItem(ctx context.Context, item Item, owner string, operation uuid.UUID, expected string, before time.Time) error {
	input, err := json.Marshal(struct {
		Owner, Incarnation string
		Before             time.Time
	}{owner, expected, before})
	if err != nil {
		return err
	}
	mutation := &manifestMutation{lifecycle: true, commit: manifestCommit{ID: operation,
		Fingerprint: sha256.Sum256(input)}}
	_, err = j.manifests.editOperation(ctx, item.Ref(), false, bound{}, mutation, func(cur *Manifest) error {
		if expected != "" && cur.Incarnation != expected {
			if !before.IsZero() {
				return errOrphanChanged
			}
			return nil
		}
		mutation.effects.Deletion = true
		mutation.effects.CleanupAt = j.cfg.Now().Add(j.cfg.LateUploadWindow)
		mutation.effects.CleanupDelete = true
		keys, err := j.manifests.allocationKeys(ctx, item)
		if err != nil {
			return err
		}
		mutation.effects.Private = keys
		for obj, err := range j.cfg.Store.List(ctx, item.Prefix()) {
			if err != nil {
				return err
			}
			if obj.Key == item.PublicPrefix()+layout.ErasureFenceName {
				continue
			}
			if k, ok := layout.Parse(obj.Key); ok && k.Folder() != item.Prefix() {
				continue // a version's folder within a content's
			}
			if !before.IsZero() && obj.LastModified.After(before) {
				return errOrphanChanged
			}
			switch {
			case strings.HasPrefix(obj.Key, item.PublicPrefix()):
				mutation.effects.Public = append(mutation.effects.Public, obj.Key)
			case obj.Key != item.ManifestKey() && !slices.Contains(keys, obj.Key):
				mutation.effects.Private = append(mutation.effects.Private, obj.Key)
			}
		}
		// A reserved public output may still be on its way to S3.
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
		mutation.effects.Cancel = j.cfg.Journal.queue != nil
		if owner != "" {
			charged, err := j.uploadBytes(ctx, item.Prefix())
			if err != nil {
				return err
			}
			mutation.effects.Settlement = Settlement{Tenant: item.Ref().TenantID, Owner: owner, Delta: -charged}
		}
		cur.Deleted, cur.Hidden = true, true
		cur.Files, cur.Meta, cur.Full, cur.Deficit = []File{}, nil, false, 0
		return nil
	})
	return err
}

// repeatDeletion never lists the folder again. A final late-upload pass can
// safely run after reset because these physical names are permanently retired.
func (j *Jobs) repeatDeletion(ctx context.Context, item Item, operation uuid.UUID) error {
	unlock, err := j.cfg.Locker.Lock(ctx, item.ManifestKey())
	if err != nil {
		return err
	}
	defer unlock()
	if _, _, err := j.manifests.recoverLocked(ctx, item); err != nil {
		return err
	}
	var c manifestCommit
	jr := j.cfg.Journal
	err = pgx.BeginFunc(ctx, jr.pool, func(tx pgx.Tx) error {
		var err error
		c, err = jr.load(ctx, tx, item.Ref().TenantID, operation)
		return err
	})
	if err != nil {
		return err
	}
	if !c.Ref.Equal(item.Ref()) || c.Folder != item.Prefix() || c.State != "applied" {
		return ErrCommitPending
	}
	current, _, err := j.manifests.get(ctx, item.ManifestKey())
	if errors.Is(err, ErrNotFound) {
		current = &Manifest{}
	} else if err != nil {
		return err
	}
	// Immutable allocations cannot be adopted after retirement. Public
	// stable aliases can be reused, so recheck their current ownership while
	// holding the same lease as a writer. Never replay a historical mirror.
	kept := j.keeps(item, current)
	c.Effects.Public = slices.DeleteFunc(slices.Clone(c.Effects.Public), func(key string) bool {
		return kept[layout.AreaPublic+"/"+strings.TrimPrefix(key, item.PublicPrefix())]
	})
	for _, key := range c.Effects.Private {
		parsed, ok := layout.Parse(key)
		if ok && kept[parsed.Area+"/"+parsed.Name] {
			return errors.New("media: retired allocation is still referenced")
		}
	}
	c.Effects.Mirror = nil
	return j.manifests.finishCommit(ctx, item, c, true, current)
}

func (j *Jobs) resetItem(ctx context.Context, item Item, deletion uuid.UUID) error {
	if _, err := j.cfg.Store.Head(ctx, item.PublicPrefix()+layout.ErasureFenceName); err == nil {
		return errors.New("media: permanently erased item cannot be reset")
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	operation := uuid.NewSHA1(deletion, []byte("reset"))
	mutation := &manifestMutation{lifecycle: true, commit: manifestCommit{ID: operation,
		Fingerprint: sha256.Sum256([]byte("reset:" + deletion.String()))}}
	_, err := j.manifests.editOperation(ctx, item.Ref(), true, bound{}, mutation, func(cur *Manifest) error {
		if !cur.Deleted || cur.Receipt == nil || cur.Receipt.Operation != deletion.String() {
			return errors.New("media: reset requires its settled deletion tombstone")
		}
		cur.Deleted = false
		cur.Incarnation = uuid.NewString()
		return nil
	})
	return err
}
