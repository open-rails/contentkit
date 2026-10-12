package media_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/token"
)

// newBoundFixture is a second app, in its own schema and bucket, that writes
// owner's shared namespace through owner's journal and bucket.
func newBoundFixture(t *testing.T, owner *fixture) *fixture {
	t.Helper()
	env := s3test.Open(t)
	f := &fixture{t: t, env: env, ns: env.Tenant, shared: owner.shared, res: owner.res, auth: owner.auth,
		purged: make(chan []string, 64), editor: owner.editor}
	cfg := testConfig(f.ns, f.shared)
	cfg.Hooks = media.Hooks{Resolver: f.res, CanUpload: f.auth}
	var err error
	if f.reg, err = media.NewRegistry(cfg); err != nil {
		t.Fatal(err)
	}
	journal, processing := env.Processing(f.reg)
	if err := journal.Bind(f.shared, owner.journal); err != nil {
		t.Fatal(err)
	}
	store, err := media.RouteStore(env.Store, map[string]media.Store{f.shared: owner.env.Store})
	if err != nil {
		t.Fatal(err)
	}
	f.journal = journal
	f.q = &queue{Queue: processing, t: t, pool: env.Pool()}
	if f.jobs, err = media.NewJobs(media.JobsConfig{Store: store, Registry: f.reg, Locker: s3test.Locker(t, store),
		Journal: journal, Processes: f.q, Pool: env.Pool()}); err != nil {
		t.Fatal(err)
	}
	f.ms = f.jobs.Manifests()
	ring, _ := token.NewRing(signKey, nil)
	if f.up, err = media.NewUploads(media.UploadOptions{Store: store, Manifests: f.ms, Tickets: &ring}); err != nil {
		t.Fatal(err)
	}
	return f
}

// Two apps write one shared namespace's avatars. The second binds it to the
// owner's journal and bucket, so each accepts the other's commits and both
// read the same current image (contentkit#115).
func TestBoundNamespaceSharesOwnerJournal(t *testing.T) {
	owner := newFixture(t)
	other := newBoundFixture(t, owner)
	ctx := t.Context()
	ref := owner.ref("user", 1)
	owner.visible(1)
	owner.put(ref, "avatar", "image/png", png(1))
	other.put(ref, "avatar", "image/png", png(2))
	owner.put(ref, "avatar", "image/png", png(3))
	other.put(ref, "avatar", "image/png", png(4))
	owner.produce(ref)

	var own int
	if err := other.env.Pool().QueryRow(ctx, `SELECT count(*) FROM `+other.env.ContentSchema()+`.content_media_commits
WHERE tenant_id = $1`, owner.shared).Scan(&own); err != nil || own != 0 {
		t.Fatalf("the bound app kept %d commits of its own (%v)", own, err)
	}
	item, _ := owner.reg.Item(ref)
	if _, err := other.env.Store.Head(ctx, item.ManifestKey()); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("the shared item reached the bound app's bucket: %v", err)
	}
	mine, err := owner.ms.PublicImages(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := other.ms.PublicImages(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) == 0 || !reflect.DeepEqual(mine, theirs) {
		t.Fatalf("owner reads %+v, bound app %+v", mine, theirs)
	}
	if _, err := other.jobs.SweepOrphans(ctx, media.OrphanSweep{Kind: "user", Exists: func(context.Context, []string) (map[string]bool, error) { return nil, nil }}); err == nil {
		t.Fatal("the bound app swept its owner's namespace")
	}
}
