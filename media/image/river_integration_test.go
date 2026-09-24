package image_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

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
	e.riverJobs(t)

	ref := contentref.NewVersion(e.Tenant, "gallery", "6", "en")
	e.commit(t, ref, ins("001.png", e.upload(t, ref, "", pngImage(t, 300, 400, 12))))
	slot := contentref.New(e.Tenant, "gallery", "6")
	e.upload(t, slot, "cover", pngImage(t, 300, 400, 13))

	deadline := time.Now().Add(45 * time.Second)
	for {
		m, _, err := e.manifests.Get(ctx, ref)
		_, cerr := e.Env.Store.Head(ctx, e.Tenant+"/gallery/6/public/cover_150.webp")
		if err == nil && len(m.Files[0].Variants) == 3 && m.Downloads["zip"].Blob != "" && cerr == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("River did not process the commit: %+v %v", m, cerr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// riverJobs makes Jobs the uploads' ProcessQueue, with Process registered,
// on a started River client carrying hooks.
func (e *env) riverJobs(t *testing.T, hooks ...rivertype.Hook) {
	t.Helper()
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
		FetchCooldown: 50 * time.Millisecond, Hooks: hooks}, jobs.RiverJobs())
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

}

// An edit that lands after a slot job's last read, while River still has the
// job running, is absorbed by it as a duplicate; it must still be rendered.
func TestRiverSlotEditLandingAsTheJobFinishes(t *testing.T) {
	e := newEnv(t, galleryKind())
	ref := contentref.New(e.Tenant, "gallery", "7")
	late := crop(0, 0, 300, 100)
	edited := make(chan error, 1)
	var once sync.Once
	e.riverJobs(t, river.HookWorkEndFunc(func(ctx context.Context, job *rivertype.JobRow, err error) error {
		if job.Kind == "contentkit_media_process" && err == nil {
			once.Do(func() { edited <- e.editSlot(t, ref, "cover", late) })
		}
		return err
	}))
	e.slot(t, ref, "cover", pngImage(t, 300, 400, 13), nil)
	select {
	case err := <-edited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("the slot job did not run")
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		m := e.slotManifest(t, ref, "cover")
		if !m.Pending && m.Edit.Hash() == late.Hash() {
			if !slices.Equal(widths(m), []int{150, 300, 300}) { // the 600 rung capped at the 300 px edit
				t.Fatalf("outputs %v", widths(m))
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the late edit was never rendered: %+v", m)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
