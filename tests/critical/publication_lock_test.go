package critical_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	versionstore "github.com/open-rails/contentkit/internal/versionstore"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
)

// Scheduling owns only durable intent. The app's real publisher, rather than
// this producer-only service, must apply media and refresh reader facts.
type schedulingOnlyFacets struct{}

func (schedulingOnlyFacets) RefreshTx(context.Context, pgx.Tx, []string) error {
	return errors.New("scheduling unexpectedly refreshed publication facts")
}

func TestPublicationSchedulingLocksParentBeforeRequest(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a, func(c *ck.Config) {
		c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	admin := a.engine.Client()
	ref := galleryRef(t, admin, "Concurrent durable publisher")
	_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 59))}})
	must(t, err)
	awaitMediaReady(t, admin, ref)
	_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, a, admin, ref.ID, ref.Version)
	awaitPublicationJobsIdle(t, w, a)
	tenant := a.cfg.Scope.AppID.String()
	service, err := versionstore.New(versionstore.Options{Tenant: tenant, Schema: w.schema, Pool: w.pool, Kinds: a.cfg.Content,
		Auth: a.deps.Auth, Resolver: &completedReaderAuthority{}, Facets: schedulingOnlyFacets{}, Now: w.clock.now})
	must(t, err)
	// Bind its normal River producer without starting a second worker fleet.
	// The actual app consumes and completes every accepted request below.
	_, err = riverhelpers.New(t.Context(), w.pool, &river.Config{Schema: w.schema + "_river", Queues: map[string]river.QueueConfig{}}, service.RiverJobs())
	must(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	writer, err := w.pool.Begin(ctx)
	must(t, err)
	defer writer.Rollback(context.Background())
	_, err = writer.Exec(ctx, `SELECT FROM `+pgx.Identifier{w.schema, "content_items"}.Sanitize()+` WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenant, ref.ID)
	must(t, err)
	_, err = writer.Exec(ctx, `SELECT FROM `+pgx.Identifier{w.schema, "content_versions"}.Sanitize()+` WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenant, ref.Version)
	must(t, err)
	writerPID := int32(writer.Conn().PgConn().PID())
	scheduler, err := w.pool.Begin(ctx)
	must(t, err)
	defer scheduler.Rollback(context.Background())
	schedulerPID := int32(scheduler.Conn().PgConn().PID())
	done := make(chan error, 1)
	go func() {
		err := service.SchedulePublicationTx(ctx, scheduler, []versions.Target{{Content: ref.ID, Version: ref.Version}})
		if err == nil {
			err = scheduler.Commit(ctx)
		} else {
			_ = scheduler.Rollback(ctx)
		}
		done <- err
	}()
	// Observe the actual blocked session, not a sleep that merely hopes the
	// scheduler has started. It must wait without owning the current request.
	waitForWithin(t, 10*time.Second, func() bool {
		var blocked bool
		must(t, w.pool.QueryRow(ctx, `SELECT $1=ANY(pg_blocking_pids($2))`, writerPID, schedulerPID).Scan(&blocked))
		return blocked
	})
	var previous string
	err = writer.QueryRow(ctx, `SELECT id::text FROM `+pgx.Identifier{w.schema, "content_publication_requests"}.Sanitize()+`
 WHERE tenant_id=$1 AND version_id=$2 AND current FOR UPDATE`, tenant, ref.Version).Scan(&previous)
	if err != nil {
		_ = writer.Rollback(ctx)
		t.Fatalf("catalog writer could not lock its request while holding the parent: %v; scheduler: %v", err, <-done)
	}
	must(t, writer.Commit(ctx))
	must(t, <-done)
	var accepted string
	must(t, w.pool.QueryRow(ctx, `SELECT id::text FROM `+pgx.Identifier{w.schema, "content_publication_requests"}.Sanitize()+`
 WHERE tenant_id=$1 AND version_id=$2 AND current`, tenant, ref.Version).Scan(&accepted))
	if accepted == previous {
		t.Fatal("scheduler never accepted a new durable reconciliation")
	}
	awaitPublicationJobsIdle(t, w, a)
	var state string
	must(t, w.pool.QueryRow(t.Context(), `SELECT state FROM `+pgx.Identifier{w.schema, "content_publication_requests"}.Sanitize()+` WHERE tenant_id=$1 AND id=$2::uuid`, tenant, accepted).Scan(&state))
	if state != "completed" {
		t.Fatalf("accepted reconciliation did not complete: %s", state)
	}
	read, err := a.client("").GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
	must(t, err)
	if len(read.Files) != 1 || read.Files[0].URL == "" {
		t.Fatalf("completed reconciliation lost reader media: %+v", read)
	}
	response, err := http.Get(read.Files[0].URL)
	must(t, err)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("completed media returned HTTP %d", response.StatusCode)
	}
}
