package critical_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/media/workqueue"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/media"
)

// revocableUploads grants every upload until revoked.
type revocableUploads struct{ revoked atomic.Bool }

func (u *revocableUploads) CanUpload(context.Context, access.Actor, media.UploadTarget) (media.UploadGrant, error) {
	return media.UploadGrant{Allowed: !u.revoked.Load()}, nil
}

// revokingLocker, once armed, revokes upload authority as a commit takes the
// item's lock: a revocation landing while the commit waited for it.
type revokingLocker struct {
	mediastore.Locker
	uploads *revocableUploads
	armed   atomic.Bool
}

func (l *revokingLocker) Lock(ctx context.Context, key string) (func(), error) {
	if l.armed.Load() {
		l.uploads.revoked.Store(true)
	}
	return l.Locker.Lock(ctx, key)
}

// A commit is authorized again under the item's lock, so authority revoked
// while it waited writes nothing.
func TestCommitReauthorizesUnderLock(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	storage := attachMedia(t, w, a)
	ref := galleryRef(t, a.engine.Client(), "Revoked while waiting")
	config, err := a.cfg.MediaKinds()
	must(t, err)
	uploads := &revocableUploads{}
	registry, err := mediastore.NewRegistry(config, mediastore.Hooks{CanUpload: uploads})
	must(t, err)
	queue, err := workqueue.New(w.pool, registry, a.cfg.Media.WorkerSchema)
	must(t, err)
	journal, err := mediastore.NewPGJournal(w.pool, w.schema, queue)
	must(t, err)
	locker := &revokingLocker{Locker: mediastore.PGLocker(w.pool), uploads: uploads}
	manifests, err := mediastore.NewManifests(storage.store, registry, mediastore.ManifestOptions{Locker: locker, Journal: journal})
	must(t, err)
	committer, err := mediastore.NewUploads(mediastore.UploadOptions{Store: storage.store, Manifests: manifests})
	must(t, err)
	target, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
	must(t, err)
	editor := access.Actor{Kind: "user", ID: uuid.NewString()}
	note := func(v string) []media.Op { return []media.Op{{Op: media.OpMeta, Meta: map[string]any{"note": v}}} }
	_, err = committer.Commit(t.Context(), editor, target, uuid.NewString(), note("kept"))
	must(t, err)
	locker.armed.Store(true)
	_, err = committer.Commit(t.Context(), editor, target, uuid.NewString(), note("revoked"))
	var refused *media.UploadError
	if !errors.As(err, &refused) || refused.Code != media.CodeForbidden {
		t.Fatalf("a commit whose authority was revoked before it took the lock answered %v", err)
	}
	current, _, err := manifests.Get(t.Context(), target)
	must(t, err)
	if current.Meta["note"] != "kept" {
		t.Fatalf("the revoked commit wrote %+v", current.Meta)
	}
}
