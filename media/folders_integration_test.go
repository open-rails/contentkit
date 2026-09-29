package media_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

var errReleaseInjected = errors.New("injected release failure")
var errDeleteInjected = errors.New("injected delete failure")

type failingDeleteStore struct {
	media.Store
	failed atomic.Bool
}

func (s *failingDeleteStore) Delete(ctx context.Context, key string) error {
	if !s.failed.Swap(true) {
		return errDeleteInjected
	}
	return s.Store.Delete(ctx, key)
}

type failingReleaser struct {
	media.QuotaReleaser
	after  bool
	failed atomic.Bool
	calls  atomic.Int32
}

func (r *failingReleaser) Release(ctx context.Context, tenant, operation string) error {
	r.calls.Add(1)
	if !r.failed.Swap(true) {
		if !r.after {
			return errReleaseInjected
		}
		if err := r.QuotaReleaser.Release(ctx, tenant, operation); err != nil {
			return err
		}
		return errReleaseInjected
	}
	return r.QuotaReleaser.Release(ctx, tenant, operation)
}

func TestPurgeReleasesQuotaAcrossRetries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		after      bool
		failDelete bool
	}{
		{name: "failure before settlement"},
		{name: "failure after settlement", after: true},
		{name: "failure deleting objects", failDelete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := s3test.Open(t)
			ctx := context.Background()
			pool := pgtest.Pool(t, nil)
			limiter, err := media.NewPGLimiter(pool, pgtest.Schema(t, ctx, pool), media.PGLimits{})
			if err != nil {
				t.Fatal(err)
			}
			if err := limiter.Settle(ctx, media.Settlement{Tenant: env.Tenant, Owner: "owner", Delta: 100}); err != nil {
				t.Fatal(err)
			}
			kinds := registry(t)
			manifests := s3test.Manifests(t, env.Store, kinds, media.ManifestOptions{})
			ref := contentref.New(env.Tenant, "post", contentref.NewID())
			item, _ := kinds.Item(ref)
			if _, err := manifests.Create(ctx, ref); err != nil {
				t.Fatal(err)
			}
			body := strings.Repeat("o", 70)
			original := blobName(body)
			key, _ := item.Original(original)
			putObject(t, env.Store, key, body)
			if _, err := manifests.Edit(ctx, ref, func(m *media.Manifest) error {
				m.Files = []media.File{{Name: "file", Original: original, Type: "image/png", Size: int64(len(body))}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var store media.Store = env.Store
			var releaser media.QuotaReleaser = limiter
			if tc.failDelete {
				store = &failingDeleteStore{Store: store}
			} else {
				releaser = &failingReleaser{QuotaReleaser: limiter, after: tc.after}
			}
			jobs, err := media.NewJobs(media.JobsConfig{Store: store, Kinds: kinds,
				Locker: s3test.Locker(t, store), Limiter: releaser})
			if err != nil {
				t.Fatal(err)
			}
			deletion := media.Deletion{Ref: ref, Owner: "owner", OperationID: "purge-request"}
			if err := jobs.Purge(ctx, media.Deletion{Ref: ref, Owner: "owner"}); err == nil {
				t.Fatal("purge without an operation id deleted the folder")
			}
			err = jobs.Purge(ctx, deletion)
			want := errReleaseInjected
			if tc.failDelete {
				want = errDeleteInjected
			}
			if !errors.Is(err, want) {
				t.Fatalf("first purge: %v", err)
			}
			if keys := listKeys(t, env.Store, item.Prefix()); !tc.failDelete && len(keys) != 0 {
				t.Fatalf("purge left %v", keys)
			}
			otherDeletion := deletion
			otherDeletion.OperationID = "second-request"
			err = jobs.Purge(ctx, otherDeletion)
			if tc.after {
				if err != nil {
					t.Fatalf("second purge after settlement: %v", err)
				}
			} else if !errors.Is(err, media.ErrQuotaReleasePending) {
				t.Fatalf("second purge with a pending refund: %v", err)
			}
			if err := jobs.Purge(ctx, deletion); err != nil {
				t.Fatalf("retry purge: %v", err)
			}
			if keys := listKeys(t, env.Store, item.Prefix()); len(keys) != 0 {
				t.Fatalf("retry left %v", keys)
			}
			if err := jobs.Purge(ctx, otherDeletion); err != nil {
				t.Fatalf("second purge of an empty folder: %v", err)
			}
			used, _, err := limiter.Usage(ctx, env.Tenant, "owner")
			if err != nil || used != 30 {
				t.Fatalf("replayed purge: used=%d err=%v", used, err)
			}
			if _, err := manifests.Create(ctx, ref); err != nil {
				t.Fatal(err)
			}
			if err := jobs.Purge(ctx, deletion); err != nil {
				t.Fatalf("late retry: %v", err)
			}
			if _, _, err := manifests.Get(ctx, ref); err != nil {
				t.Fatalf("late retry removed a new manifest: %v", err)
			}
		})
	}
}

func TestContentIDsMustBeUUIDv7(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	r := registry(t)
	ms := s3test.Manifests(t, env.Store, r, media.ManifestOptions{})
	for _, id := range []string{"18", uuid.NewString(), "0192F000-0000-7000-8000-000000000001"} {
		ref := contentref.New(env.Tenant, "post", id)
		if _, err := r.Item(ref); !errors.Is(err, contentref.ErrInvalidID) {
			t.Fatalf("Item(%q): %v", id, err)
		}
		if _, err := ms.Create(ctx, ref); !errors.Is(err, contentref.ErrInvalidID) {
			t.Fatalf("Create(%q): %v", id, err)
		}
		_, err := ms.Edit(ctx, ref, func(*media.Manifest) error { return nil })
		if ue, ok := media.AsUploadError(err); !errors.Is(err, contentref.ErrInvalidID) || !ok || ue.Code != media.CodeInvalid {
			t.Fatalf("Edit(%q): %v", id, err)
		}
	}
}

func TestNewItemNeverAdoptsLeftovers(t *testing.T) {
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	ctx := context.Background()
	r := registry(t)
	ms := s3test.Manifests(t, env.Store, r, media.ManifestOptions{})
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Locker: s3test.Locker(t, env.Store), Kinds: r, Tenants: []string{env.Tenant}})
	if err != nil {
		t.Fatal(err)
	}
	notEmpty := func(what string, err error, prefix string) {
		t.Helper()
		var fe *media.FolderNotEmptyError
		if !errors.Is(err, media.ErrFolderNotEmpty) || !errors.As(err, &fe) || fe.Prefix != prefix || len(fe.Keys) == 0 {
			t.Fatalf("%s: %v", what, err)
		}
		if ue, ok := media.AsUploadError(err); !ok || ue.Code != media.CodeConflict {
			t.Fatalf("%s: upload error %v", what, ue)
		}
	}

	// Create writes an empty manifest into an empty folder, once.
	fresh := contentref.New(env.Tenant, "post", contentref.NewID())
	item, _ := r.Item(fresh)
	if man, err := ms.Create(ctx, fresh); err != nil || len(man.Files) != 0 {
		t.Fatalf("create: %+v %v", man, err)
	}
	if man, _, err := ms.Get(ctx, fresh); err != nil || len(man.Files) != 0 {
		t.Fatalf("created manifest: %+v %v", man, err)
	}
	_, err = ms.Create(ctx, fresh)
	notEmpty("second create", err, item.Prefix())

	// A reused id: any leftover object refuses Create until the host purges.
	reused := contentref.New(env.Tenant, "post", contentref.NewID())
	old, _ := r.Item(reused)
	putObject(t, env.Store, old.OriginalsPrefix()+blobName("old"), "old upload")
	putObject(t, env.Store, old.PublicPrefix()+blobName("old poster"), "old poster")
	_, err = ms.Create(ctx, reused)
	notEmpty("create over leftovers", err, old.Prefix())
	if err := jobs.Purge(ctx, media.Deletion{Ref: reused}); err != nil {
		t.Fatal(err)
	}
	if keys := listKeys(t, env.Store, old.Prefix()); len(keys) != 0 {
		t.Fatalf("purge left %v", keys)
	}
	if _, err := ms.Create(ctx, reused); err != nil {
		t.Fatalf("create after purge: %v", err)
	}
	if err := jobs.Purge(ctx, media.Deletion{Ref: reused.WithVersion("v1")}); err == nil {
		t.Fatal("purge of a version accepted")
	}

	// Without Create, a folder's first manifest refuses a previous item's
	// blobs, but not uploads waiting for their first commit.
	stale := contentref.New(env.Tenant, "post", contentref.NewID())
	s, _ := r.Item(stale)
	putObject(t, env.Store, s.PrivatePrefix()+blobName("old variant"), "old variant")
	putObject(t, env.Store, s.OriginalsPrefix()+blobName("new"), "new upload")
	insert := func(m *media.Manifest) error {
		m.Files = append(m.Files, media.File{Name: "a.png", Original: blobName("new"), Type: "image/png"})
		return nil
	}
	_, err = ms.Edit(ctx, stale, insert)
	notEmpty("first edit over blobs", err, s.Prefix())
	if _, _, err := ms.Get(ctx, stale); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("refused edit wrote a manifest: %v", err)
	}
	pending := contentref.New(env.Tenant, "post", contentref.NewID())
	p, _ := r.Item(pending)
	putObject(t, env.Store, p.OriginalsPrefix()+blobName("new"), "new upload")
	if man, err := ms.Edit(ctx, pending, insert); err != nil || len(man.Files) != 1 {
		t.Fatalf("first commit of pending uploads: %+v %v", man, err)
	}
	// Versioned kinds: a new version beside existing ones is fine.
	g := contentref.New(env.Tenant, "gallery", contentref.NewID())
	gi, _ := r.Item(g)
	if _, err := ms.Edit(ctx, g.WithVersion("v1"), insert); err != nil {
		t.Fatal(err)
	}
	putObject(t, env.Store, gi.PrivatePrefix()+blobName("v1 variant"), "v1 variant")
	if _, err := ms.Edit(ctx, g.WithVersion("v2"), insert); err != nil {
		t.Fatalf("second version: %v", err)
	}
}

func TestSweepOrphansReportsThenDeletesAfterGrace(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	r := registry(t)
	clock := time.Now().Add(2 * time.Hour)
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Locker: s3test.Locker(t, env.Store), Kinds: r, Tenants: []string{env.Tenant}, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	kept, gone := contentref.NewID(), contentref.NewID()
	folder := func(id string) string { return env.Tenant + "/post/" + id + "/" }
	for _, id := range []string{kept, gone, "18"} {
		putObject(t, env.Store, folder(id)+"manifest.json", `{"files":[]}`)
		putObject(t, env.Store, folder(id)+"originals/"+blobName(id), id)
	}
	other := env.Tenant + "/gallery/" + gone + "/manifest.json"
	putObject(t, env.Store, other, `{"files":[]}`)
	var asked []string
	exists := func(_ context.Context, ids []string) (map[string]bool, error) {
		asked = append(asked, ids...)
		return map[string]bool{kept: true}, nil
	}
	sweep := media.OrphanSweep{Tenant: env.Tenant, Kind: "post", Exists: exists, Grace: time.Hour}

	rep, err := jobs.SweepOrphans(ctx, sweep)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(rep media.OrphanReport) []string {
		var out []string
		for _, o := range rep.Orphans {
			out = append(out, o.ID)
			if o.Deleted != sweep.Delete || o.Objects != 2 || o.ValidID != (o.ID != "18") || o.Prefix != folder(o.ID) {
				t.Fatalf("orphan %+v", o)
			}
		}
		slices.Sort(out)
		return out
	}
	want := []string{gone, "18"}
	slices.Sort(want)
	if rep.Folders != 3 || !slices.Equal(ids(rep), want) {
		t.Fatalf("report %+v", rep)
	}
	if slices.Contains(asked, "18") || len(asked) != 2 {
		t.Fatalf("host asked about %v", asked)
	}
	if n := len(listKeys(t, env.Store, env.Tenant+"/post/")); n != 6 {
		t.Fatalf("a report deleted objects: %d left", n)
	}

	// Within the grace period nothing is an orphan yet.
	sweep.Grace = 3 * time.Hour
	if rep, err := jobs.SweepOrphans(ctx, sweep); err != nil || len(rep.Orphans) != 0 || rep.Folders != 3 {
		t.Fatalf("fresh folders: %+v %v", rep, err)
	}

	sweep.Grace, sweep.Delete = time.Hour, true
	if rep, err = jobs.SweepOrphans(ctx, sweep); err != nil || !slices.Equal(ids(rep), want) {
		t.Fatalf("delete: %+v %v", rep, err)
	}
	left := listKeys(t, env.Store, env.Tenant+"/")
	if len(left) != 3 || !slices.Contains(left, other) || !slices.Contains(left, folder(kept)+"manifest.json") {
		t.Fatalf("left %v", left)
	}
	if _, err := jobs.SweepOrphans(ctx, media.OrphanSweep{Tenant: env.Tenant, Kind: "nope", Exists: exists}); !errors.Is(err, media.ErrUnknownKind) {
		t.Fatalf("unknown kind: %v", err)
	}
}
