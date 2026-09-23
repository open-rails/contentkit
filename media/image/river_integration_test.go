package image_test

import (
	"context"
	"testing"
	"time"

	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/image"
)

// TestRiverProcessing wires the processor as a host does: Jobs is the
// uploads' ProcessQueue and Process its registered processor.
func TestRiverProcessing(t *testing.T) {
	e := newEnv(t, galleryKind())
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	kinds, err := media.NewRegistry(galleryKind())
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := media.NewJobs(media.JobsConfig{Store: e.Env.Store, Kinds: kinds})
	if err != nil {
		t.Fatal(err)
	}
	proc, err := image.New(image.Config{Store: e.Env.Store, Kinds: kinds, Manifests: e.manifests})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.AddProcessor(proc.Process); err != nil {
		t.Fatal(err)
	}
	e.uploads, err = media.NewUploads(media.UploadOptions{Store: e.Env.Store, Kinds: kinds, Manifests: e.manifests,
		Authorizer: allow{}, Queue: jobs})
	if err != nil {
		t.Fatal(err)
	}
	client, err := riverhelpers.New(ctx, pool, &river.Config{Schema: schema, FetchPollInterval: 100 * time.Millisecond,
		FetchCooldown: 50 * time.Millisecond}, jobs.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.StopAndCancel(ctx)
	})

	ref := contentref.NewVersion(e.Tenant, "gallery", "6", "en")
	e.commit(t, ref, ins("001.png", e.upload(t, ref, "", pngImage(t, 300, 400, 12))))
	slot := contentref.New(e.Tenant, "gallery", "6")
	e.upload(t, slot, "cover", pngImage(t, 300, 400, 13))

	deadline := time.Now().Add(45 * time.Second)
	for {
		m, _, err := e.manifests.Get(ctx, ref)
		_, cerr := e.Env.Store.Head(ctx, e.Tenant+"/gallery/6/public/cover.webp")
		if err == nil && len(m.Files[0].Variants) == 3 && m.Downloads["zip"].Blob != "" && cerr == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("River did not process the commit: %+v %v", m, cerr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
