package critical_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	medias3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/media/workqueue"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

// Hold only the actual metadata publisher's manifest GET after it owns its
// source and catalog locks. Bytes, ownership validation and worker execution
// still use the real object store, PostgreSQL and registered River worker.
type metadataReadBarrier struct {
	*medias3.Store
	observer                                      *pgxpool.Pool
	application, sourceKey, manifestKey, rowQuery string
	armed, held                                   atomic.Bool
	arrived                                       chan int32
	release                                       chan struct{}
	once                                          sync.Once
}

func (s *metadataReadBarrier) unblock() { s.once.Do(func() { close(s.release) }) }
func (s *metadataReadBarrier) Get(ctx context.Context, key string, o media.GetOptions) (io.ReadCloser, media.Object, error) {
	if s.armed.Load() && !s.held.Load() && key == s.manifestKey {
		var pid int32
		err := s.observer.QueryRow(ctx, `SELECT a.pid FROM pg_stat_activity a
 WHERE a.application_name=$1 AND a.state='idle in transaction' AND a.query=$2
 AND EXISTS(SELECT 1 FROM pg_locks l WHERE l.locktype='advisory' AND l.granted
 AND l.classid::bigint=((hashtextextended($3,0)>>32)&4294967295)
 AND l.objid::bigint=(hashtextextended($3,0)&4294967295)) LIMIT 1`, s.application, s.rowQuery, s.sourceKey).Scan(&pid)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, media.Object{}, err
		}
		if err == nil && s.held.CompareAndSwap(false, true) {
			s.arrived <- pid
			select {
			case <-s.release:
			case <-ctx.Done():
				return nil, media.Object{}, ctx.Err()
			}
		}
	}
	return s.Store.Get(ctx, key, o)
}

type metadataPoolJob struct {
	Content string `json:"content"`
	Version string `json:"version"`
}

func (metadataPoolJob) Kind() string { return "contentkit_version_publish" }

func TestMetadataManifestReadsUseTransactions(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	storage := attachMedia(t, w, a, func(c *ck.Config) {
		c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	client := a.engine.Client()
	item, err := client.PostContents(t.Context(), catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: "Sibling metadata under bounded pool"}}, Versions: []catalog.VersionInput{{Languages: []string{"en"}}, {Languages: []string{"de"}}}})
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

	t.Run("sibling_metadata_three_connections", func(t *testing.T) {
		cfg := w.pool.Config()
		cfg.MaxConns = 3
		if cfg.ConnConfig.RuntimeParams == nil {
			cfg.ConnConfig.RuntimeParams = map[string]string{}
		}
		marker := "metadata-pool-" + uuid.NewString()
		cfg.ConnConfig.RuntimeParams["application_name"] = marker
		pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
		must(t, err)
		kinds, err := a.cfg.MediaKinds()
		must(t, err)
		registry, err := media.NewRegistry(kinds)
		must(t, err)
		ref, err := registry.VersionRef("gallery", refs[0].ID, refs[0].Version)
		must(t, err)
		mediaItem, err := registry.Item(ref)
		must(t, err)
		barrier := &metadataReadBarrier{Store: storage.store, observer: w.pool, application: marker,
			sourceKey:   "contentkit:media:contentkit:versions:" + a.cfg.Scope.AppID.String() + ":" + refs[0].Version,
			manifestKey: mediaItem.ManifestKey(), rowQuery: `SELECT FROM ` + pgx.Identifier{w.schema, "content_versions"}.Sanitize() + ` WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			arrived: make(chan int32, 1), release: make(chan struct{})}
		a.deps.Postgres, a.deps.Store = pool, barrier
		a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
		must(t, err)
		a.server = mountEngine(t, a.engine)
		queue := versions.QueueFor(a.cfg.Scope.AppID.String())
		var dormant []string
		only := riverhelpers.NewContribution("metadata-pool-only", func(_ context.Context, c *river.Config) error {
			for q := range c.Queues {
				if q != queue {
					dormant = append(dormant, q)
				}
			}
			return nil
		}, nil, nil)
		fleet, err := riverhelpers.New(t.Context(), pool, &river.Config{Schema: w.schema + "_river", Queues: map[string]river.QueueConfig{queue: {MaxWorkers: 2}}}, a.engine.RiverJobs(), only)
		must(t, err)
		for _, q := range dormant {
			must(t, fleet.Queues().Remove(t.Context(), q))
		}
		var held []*pgxpool.Conn
		t.Cleanup(func() {
			barrier.unblock()
			for _, c := range held {
				c.Release()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := fleet.StopAndCancel(ctx); err != nil {
				t.Errorf("stop owned publishers: %v", err)
			}
			if err := a.engine.Close(ctx); err != nil {
				t.Errorf("close owned metadata engine: %v", err)
			}
			pool.Close()
		})
		must(t, a.engine.Start(t.Context(), ck.WithRiverClient(fleet)))
		must(t, fleet.Start(t.Context()))
		awaitPublicationJobsIdle(t, w, a)
		producer, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
		must(t, err)
		barrier.armed.Store(true)
		leader, err := producer.Insert(t.Context(), metadataPoolJob{Content: item.ID, Version: refs[0].Version}, &river.InsertOpts{Queue: queue})
		must(t, err)
		var pid int32
		select {
		case pid = <-barrier.arrived:
		case <-time.After(15 * time.Second):
			t.Fatal("metadata publisher did not reach its held-transaction manifest read")
		}
		follower, err := producer.Insert(t.Context(), metadataPoolJob{Content: item.ID, Version: refs[1].Version}, &river.InsertOpts{Queue: queue})
		must(t, err)
		waitForWithin(t, 10*time.Second, func() bool {
			var waiting int
			must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND $2=ANY(pg_blocking_pids(pid))`, marker, pid).Scan(&waiting))
			return waiting >= 1
		})
		// Reserve the capacity normally used by the host's other database work.
		// Observe the real sibling row-lock waiter before exhausting that capacity.
		for pool.Stat().AcquiredConns() < 3 {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			c, err := pool.Acquire(ctx)
			cancel()
			must(t, err)
			held = append(held, c)
		}
		barrier.unblock()
		deadline := time.Now().Add(10 * time.Second)
		completed := false
		for time.Now().Before(deadline) {
			first, err := producer.JobGet(t.Context(), leader.Job.ID)
			must(t, err)
			second, err := producer.JobGet(t.Context(), follower.Job.ID)
			must(t, err)
			if first.State == rivertype.JobStateDiscarded || second.State == rivertype.JobStateDiscarded {
				t.Fatalf("metadata job failed: %v %v", first.Errors, second.Errors)
			}
			if first.State == rivertype.JobStateCompleted && second.State == rivertype.JobStateCompleted {
				completed = true
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		if !completed {
			t.Fatal("metadata publishers deadlocked while their held transaction needed another pool connection")
		}
		for _, c := range held {
			c.Release()
		}
		held = nil
		for _, ref := range refs {
			read, err := a.engine.Client().GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
			must(t, err)
			if len(read.Files) == 0 {
				t.Fatal("metadata completion lost the edition's media")
			}
		}
	})

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
