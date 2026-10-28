package critical_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

// The SDK reloads the reader plan every few seconds while next_release_at is
// past. A parked or failed release must not keep every open reader polling.
func TestReaderPlanReleaseHint(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a, func(c *ck.Config) {
		c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	admin, viewer := a.engine.Client(), a.client("")
	ref := galleryRef(t, admin, "Release hint")
	_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{
		{Path: "pages/01.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 9))},
		{Path: "pages/20.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 57))},
	})
	must(t, err)
	awaitMediaReady(t, admin, ref)
	future := w.clock.now().Add(time.Hour)
	_, err = admin.PutContentVersionSegments(t.Context(), ref.ID, ref.Version, catalog.SegmentsInput{Segments: []catalog.SegmentInput{
		{Position: 0, StartPage: "pages/01.png"},
		{Position: 1, StartPage: "pages/20.png", ReleaseAt: &future},
	}})
	must(t, err)
	_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	read := func() (*time.Time, error) {
		reply, err := viewer.GetContentVersionReader(t.Context(), ref.ID, ref.Version)
		if err != nil {
			return nil, err
		}
		var plan versions.ReaderPlan
		err = json.Unmarshal(reply.Body, &plan)
		return plan.NextReleaseAt, err
	}
	hint := func() *time.Time {
		t.Helper()
		at, err := read()
		must(t, err)
		return at
	}
	atFuture := func(at *time.Time) bool { return at != nil && at.Sub(future).Abs() < time.Millisecond }
	waitForWithin(t, 45*time.Second, func() bool {
		at, err := read()
		return err == nil && atFuture(at)
	})
	waitForWithin(t, 45*time.Second, func() bool {
		var due int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize()+`
 WHERE kind='contentkit_publication' AND state IN ('available','pending','running','retryable') AND scheduled_at<=clock_timestamp()`).Scan(&due))
		return due == 0
	})

	requests := pgx.Identifier{w.schema, "content_publication_requests"}.Sanitize()
	past := w.clock.now().Add(-time.Minute)
	for _, c := range []struct {
		state string
		want  bool
	}{{"waiting", false}, {"failed", false}, {"pending", true}, {"running", true}} {
		tag, err := w.pool.Exec(t.Context(), `UPDATE `+requests+` SET state=$3,scheduled_at=$4 WHERE tenant_id=$1 AND version_id=$2 AND current`,
			a.cfg.Scope.AppID.String(), ref.Version, c.state, past)
		must(t, err)
		if tag.RowsAffected() != 1 {
			t.Fatalf("%s: updated %d current requests", c.state, tag.RowsAffected())
		}
		if got := hint(); (got != nil) != c.want {
			t.Fatalf("%s request at a past time: next_release_at=%v, want present=%v", c.state, got, c.want)
		}
	}

	_, err = w.pool.Exec(t.Context(), `UPDATE `+requests+` SET state='pending',scheduled_at=$3 WHERE tenant_id=$1 AND version_id=$2 AND current`,
		a.cfg.Scope.AppID.String(), ref.Version, future)
	must(t, err)
	if got := hint(); !atFuture(got) {
		t.Fatalf("future chapter date: next_release_at=%v, want %v", got, future)
	}
}
