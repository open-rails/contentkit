package critical_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/media/workqueue"
)

func TestVersionReadinessUsesHeldTransaction(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	storage := attachMedia(t, w, a, func(c *ck.Config) {
		c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	client := a.engine.Client()
	item, err := client.PostContents(t.Context(), catalog.ContentInput{Kind: "gallery", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Sibling metadata under bounded pool"}}}, {Languages: []string{"de"}, Text: map[string]catalog.VersionText{"en": {Title: "Sibling metadata under bounded pool"}}}}})
	must(t, err)
	refs := []media.RefBody{{Kind: "gallery", ID: item.ID, Version: item.Versions[0].ID}, {Kind: "gallery", ID: item.ID, Version: item.Versions[1].ID}}
	for i, ref := range refs {
		_, err = client.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, byte(71+i)))}})
		must(t, err)
		awaitMediaReady(t, client, ref)
	}
	_, err = client.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	for _, ref := range refs {
		awaitEditionPublication(t, a, client, ref.ID, ref.Version)
	}
	awaitPublicationJobsIdle(t, w, a)
	a.server.Close()
	must(t, a.engine.Close(t.Context()))

	t.Run("pending_read_single_connection", func(t *testing.T) {
		cfg := w.pool.Config()
		cfg.MaxConns = 1
		pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
		must(t, err)
		defer pool.Close()
		service, err := versions.New(versions.Options{Tenant: a.cfg.Scope.AppID.String(), Schema: w.schema, Pool: pool, Kinds: a.cfg.Content, Auth: a.deps.Auth, Resolver: &completedReaderAuthority{}, Facets: schedulingOnlyFacets{}})
		must(t, err)
		kinds, err := a.cfg.MediaKinds()
		must(t, err)
		kinds.Hooks = service.Hooks()
		registry, err := media.NewRegistry(kinds)
		must(t, err)
		queue, err := workqueue.New(pool, registry, a.cfg.Media.WorkerSchema)
		must(t, err)
		journal, err := media.NewPGJournal(pool, w.schema, queue)
		must(t, err)
		jobs, err := media.NewJobs(media.JobsConfig{Store: storage.store, Registry: registry, Locker: media.PGLocker(pool), Journal: journal, Processes: queue})
		must(t, err)
		uploads, err := media.NewUploads(media.UploadOptions{Store: storage.store, Manifests: jobs.Manifests()})
		must(t, err)
		must(t, service.Bind(versions.Media{Manifests: jobs.Manifests(), Uploads: uploads, Jobs: jobs}))
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		tx, err := pool.Begin(ctx)
		must(t, err)
		defer tx.Rollback(context.Background())
		pending, err := service.PendingTx(ctx, tx, []versions.Target{{Content: item.ID, Version: refs[0].Version}, {Content: item.ID, Version: refs[1].Version}})
		must(t, err)
		if len(pending) != 0 {
			t.Fatalf("completed sibling media is unexpectedly pending: %+v", pending)
		}
		must(t, tx.Commit(ctx))
	})
}
