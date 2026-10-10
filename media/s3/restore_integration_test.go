package s3_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
)

func TestRestoreAfterSweepAndFolderDeletion(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	if env.Created {
		if err := env.Store.Configure(ctx, 30); err != nil {
			t.Fatal(err)
		}
	}
	images := []string{"image/png"}
	kinds, err := media.NewRegistry(media.Config{Namespace: env.Tenant, Kinds: []media.Kind{
		{Name: "channel", KeepOriginals: true, Uploads: []media.Upload{{Path: "files/{name}", Types: images, MaxBytes: 1 << 20}},
			Private: []media.Private{{Name: "v", From: "files/{name}", To: "v/{name}.webp", Image: &media.Image{}}}},
		{Name: "post", KeepOriginals: true, Uploads: []media.Upload{{Path: "files/{name}", Types: images, MaxBytes: 1 << 20}},
			Private: []media.Private{{Name: "v", From: "files/{name}", To: "v/{name}.webp", Image: &media.Image{}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	store := &restoreStore{Store: env.Store}
	journal, queue := env.Processing(kinds)
	limiter, err := media.NewPGLimiter(env.Pool(), env.ContentSchema(), media.PGLimits{Quota: func(context.Context, string, string) (int64, error) {
		return 1 << 20, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	const grace = 24 * time.Hour
	clock := time.Now()
	jobs, err := media.NewJobs(media.JobsConfig{Store: store, Locker: s3test.Locker(t, store), Journal: journal, Registry: kinds,
		Processes: queue, Limiter: limiter, Grace: grace, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	ms := jobs.Manifests()
	channel := contentref.New(env.Tenant, "channel", cid(1))
	ch, _ := kinds.Item(channel)
	post := contentref.New(env.Tenant, "post", cid(2))
	p, _ := kinds.Item(post)
	seed := func(ref contentref.ContentRef, original, output string) []string {
		t.Helper()
		item, _ := kinds.Item(ref)
		names := make([]string, 2)
		for i, body := range []string{original, output} {
			sum := sha256.Sum256([]byte(body))
			name, err := ms.NewBlob(ctx, ref, sum[:])
			if err != nil {
				t.Fatal(err)
			}
			names[i] = name
			key, err := item.Blob(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Put(ctx, key, strings.NewReader(body), int64(len(body)), media.PutOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := ms.Edit(ctx, ref, func(m *media.Manifest) error {
			m.Files = []media.File{{Path: "files/f.png", Blob: names[0], Type: "image/png"},
				{Path: "v/f.webp", Blob: names[1], Type: "image/webp", From: "files/f.png", Preset: "v", FP: "x"}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return names
	}
	old := seed(channel, "origA", "outA")
	seed(post, "origP", "outP")
	initial, _, err := ms.Get(ctx, channel)
	if err != nil {
		t.Fatal(err)
	}
	if err := limiter.Settle(ctx, media.Settlement{Tenant: env.Tenant, Owner: "owner", Delta: 2 * initial.UploadBytes()}); err != nil {
		t.Fatal(err)
	}
	// S3 version timestamps have second resolution. Keep later mutations in a
	// different second from the chosen historical instant.
	time.Sleep(1100 * time.Millisecond)
	at := time.Now()
	snap, err := store.Snapshot(ctx, ch, at)
	if err != nil {
		t.Fatal(err)
	}
	postSnapshot, err := store.Snapshot(ctx, p, at)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	seed(channel, "origB", "outB")
	clock = time.Now().Add(grace + time.Minute)
	res, err := jobs.Sweep(ctx, channel)
	if err != nil || len(res.Deleted) != 2 {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	deletion := media.Deletion{Ref: post, Owner: "owner", OperationID: uuid.NewString()}
	if err := jobs.Purge(ctx, deletion); err != nil {
		t.Fatal(err)
	}
	options := media.RestoreOptions{OperationID: uuid.NewString(), Owner: "owner"}
	store.loseCopy.Store(true)
	if _, err := jobs.Restore(ctx, snap, options); !errors.Is(err, media.ErrUnavailable) {
		t.Fatalf("lost copy response: %v", err)
	}
	if current, _, err := ms.Get(ctx, channel); err != nil || current.Files[0].Blob == old[0] {
		t.Fatalf("failed restore changed current root: %+v %v", current, err)
	}
	if err := ms.Recover(ctx, channel); err != nil {
		t.Fatal(err)
	}
	store.loseRoot.Store(true)
	if _, err := jobs.Restore(ctx, snap, options); !errors.Is(err, media.ErrUnavailable) {
		t.Fatalf("lost root response: %v", err)
	}
	copies := store.copies.Load()
	man, err := jobs.Restore(ctx, snap, options)
	if err != nil {
		t.Fatal(err)
	}
	if man.Incarnation == initial.Incarnation || man.Files[0].Blob == old[0] || store.copies.Load() != copies {
		t.Fatalf("restore did not isolate/recover its generation: %+v", man)
	}
	read := func(item media.Item, f media.File, want string) {
		t.Helper()
		key, err := item.Blob(f.Blob)
		if err != nil {
			t.Fatal(err)
		}
		rc, _, err := store.Get(ctx, key, media.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || string(body) != want {
			t.Fatalf("restored %s: %q %v, want %q", key, body, err, want)
		}
	}
	read(ch, man.Files[0], "origA")
	read(ch, man.Files[1], "outA")
	for _, name := range old {
		key, _ := ch.Blob(name)
		if _, err := store.Head(ctx, key); !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("restore revived retired allocation: %s %v", key, err)
		}
	}
	if _, err := ms.Edit(ctx, channel, func(m *media.Manifest) error { m.Meta = map[string]any{"saved": true}; return nil }); err != nil {
		t.Fatalf("restored references cannot be edited: %v", err)
	}
	if replay, err := jobs.Restore(ctx, snap, options); err != nil || replay.Meta["saved"] != true || replay.Incarnation != man.Incarnation {
		t.Fatalf("restore replay overwrote later edits: %+v %v", replay, err)
	}
	changed := snap
	changed.Manifest = snap.Manifest.Clone()
	changed.Manifest.Meta = map[string]any{"different": true}
	if _, err := jobs.Restore(ctx, changed, options); !errors.Is(err, media.ErrCommitIdentity) || store.copies.Load() != copies {
		t.Fatalf("changed restore identity copied or published: %v", err)
	}
	if _, err := jobs.Restore(ctx, postSnapshot, media.RestoreOptions{OperationID: uuid.NewString(), Owner: "owner"}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Purge(ctx, deletion); err != nil {
		t.Fatal(err)
	}
	restored, _, err := ms.Get(ctx, post)
	if err != nil {
		t.Fatal(err)
	}
	read(p, restored.Files[0], "origP")
	read(p, restored.Files[1], "outP")
	missing := postSnapshot.Objects[0]
	if _, err := store.Client().DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &env.Config.Bucket,
		Key: &missing.Key, VersionId: &missing.VersionID}); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Restore(ctx, postSnapshot, media.RestoreOptions{OperationID: uuid.NewString(), Owner: "owner"}); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("restore substituted a missing historical version: %v", err)
	}
	if err := ms.Recover(ctx, post); err != nil {
		t.Fatal(err)
	}
	current, _, err := ms.Get(ctx, post)
	if err != nil || current.Incarnation != restored.Incarnation {
		t.Fatalf("missing historical bytes replaced current root: %+v %v", current, err)
	}
	read(p, current.Files[0], "origP")
	read(p, current.Files[1], "outP")
	var used int64
	if err := env.Pool().QueryRow(ctx, `SELECT used_bytes FROM `+env.ContentSchema()+`.content_media_usage
WHERE tenant_id = $1 AND owner_id = $2`, env.Tenant, "owner").Scan(&used); err != nil || used != 2*initial.UploadBytes() {
		t.Fatalf("restore quota replay: %d %v", used, err)
	}
	var pending int
	if err := env.Pool().QueryRow(ctx, `SELECT count(*) FROM `+env.ContentSchema()+`.content_media_commits
WHERE state IN ('open', 'prepared', 'frozen')`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("unsettled restore attempts: %d %v", pending, err)
	}
}

// Unreadable historical roots are refused without mutating live objects. The
// caller can inspect their retained versions rather than republish unknown data.
func TestSnapshotRefusesUnreadableFolder(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	if env.Created {
		if err := env.Store.Configure(ctx, 30); err != nil {
			t.Fatal(err)
		}
	}
	kinds, err := media.NewRegistry(media.Config{Namespace: env.Tenant, Kinds: []media.Kind{
		{Name: "post", Uploads: []media.Upload{{Path: "files/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20}}}}})
	if err != nil {
		t.Fatal(err)
	}
	item, _ := kinds.Item(contentref.New(env.Tenant, "post", cid(3)))
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	zw.Write([]byte(`{"v":3,"meta":{"x":"`))
	zw.Write(bytes.Repeat([]byte("A"), media.MaxManifestBytes))
	zw.Write([]byte(`"},"files":[]}`))
	zw.Close()
	before, err := env.Store.Put(ctx, item.ManifestKey(), bytes.NewReader(body.Bytes()), int64(body.Len()), media.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store.Snapshot(ctx, item, time.Now().Add(time.Second)); !errors.Is(err, media.ErrManifestUnreadable) {
		t.Fatalf("unreadable snapshot: %v", err)
	}
	after, err := env.Store.Head(ctx, item.ManifestKey())
	if err != nil || after.ETag != before.ETag {
		t.Fatalf("snapshot mutated root: %+v %v", after, err)
	}
}

type restoreStore struct {
	*mediaS3.Store
	loseRoot atomic.Bool
	loseCopy atomic.Bool
	copies   atomic.Int64
}

func (s *restoreStore) Put(ctx context.Context, key string, body io.Reader, size int64, o media.PutOptions) (media.Object, error) {
	object, err := s.Store.Put(ctx, key, body, size, o)
	if err == nil && strings.HasSuffix(key, "/manifest.json") && s.loseRoot.CompareAndSwap(true, false) {
		return media.Object{}, media.ErrUnavailable
	}
	return object, err
}

func (s *restoreStore) CopyVersion(ctx context.Context, version media.ObjectVersion, dst string) (media.Object, error) {
	s.copies.Add(1)
	object, err := s.Store.CopyVersion(ctx, version, dst)
	if err == nil && s.loseCopy.CompareAndSwap(true, false) {
		return media.Object{}, media.ErrUnavailable
	}
	return object, err
}
