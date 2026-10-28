package critical_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	versionstore "github.com/open-rails/contentkit/internal/versionstore"
	versions "github.com/open-rails/contentkit/media/versions"
)

// Processing wake-ups and repeated publishes of a released edition re-plan it.
// A run that changes nothing must not add a request row or copy the live
// release; a real release removes the finished requests it supersedes.
func TestUnchangedReleaseLeavesNoRequestCopies(t *testing.T) {
	g := newHiddenStartGallery(t, false, []string{"pages/01.png", "pages/02.png", "pages/10.png", "pages/11.png"}, laterChapters(time.Hour))
	chapterTwo := g.start.Add(time.Hour)
	tenant := publicationTenant(g.a)
	requests := pgx.Identifier{g.w.schema, "content_publication_requests"}.Sanitize()

	config, err := g.a.cfg.MediaKinds()
	must(t, err)
	registry, err := mediastore.NewRegistry(config, mediastore.Hooks{})
	must(t, err)
	ref, err := registry.VersionRef(g.ref.Kind, g.ref.ID, g.ref.Version)
	must(t, err)
	processed, err := versionstore.ProcessingPublisher(g.w.pool, publicationRiverSchema(g.a), g.w.schema, registry)
	must(t, err)
	wake := func() {
		t.Helper()
		must(t, pgx.BeginFunc(t.Context(), g.w.pool, func(tx pgx.Tx) error { return processed(t.Context(), tx, ref) }))
	}

	type rowState struct {
		total, copies, plans int
		live, current        string
		due                  time.Time
	}
	rows := func() rowState {
		t.Helper()
		var r rowState
		must(t, g.w.pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE r.result IS NOT NULL),
 count(*) FILTER (WHERE r.plan IS NOT NULL),
 (SELECT coalesce(release_id::text,'') FROM `+pgx.Identifier{g.w.schema, "content_versions"}.Sanitize()+` WHERE tenant_id=$1 AND id=$2),
 coalesce(max(r.id::text) FILTER (WHERE r.current),''),coalesce(max(r.scheduled_at) FILTER (WHERE r.current),'epoch')
 FROM `+requests+` r WHERE r.tenant_id=$1 AND r.version_id=$2`, tenant, g.ref.Version).Scan(&r.total, &r.copies, &r.plans, &r.live, &r.current, &r.due))
		return r
	}
	// settle waits until every due release ran and the current request is
	// pending at want, or completed when want is zero.
	settle := func(want time.Time) rowState {
		t.Helper()
		var r rowState
		waitForWithin(t, 45*time.Second, func() bool {
			wakeDuePublicationJobs(t, g.a)
			var busy int
			must(t, g.w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{publicationRiverSchema(g.a), "river_job"}.Sanitize()+`
 WHERE kind='contentkit_publication' AND state IN ('available','pending','running','retryable')`).Scan(&busy))
			var state string
			err := g.w.pool.QueryRow(t.Context(), `SELECT state FROM `+requests+` WHERE tenant_id=$1 AND version_id=$2 AND current`, tenant, g.ref.Version).Scan(&state)
			r = rows()
			if busy != 0 || err != nil {
				return false
			}
			if want.IsZero() {
				return state == "completed"
			}
			return state == "pending" && r.due.Equal(want)
		})
		return r
	}

	type readerView struct {
		paths, archive []string
		plan, chapters string
	}
	view := func() readerView {
		t.Helper()
		paths, _ := g.paths(t, g.viewer)
		reply, err := g.viewer.GetContentVersionReader(t.Context(), g.ref.ID, g.ref.Version)
		must(t, err)
		if reply.Status != http.StatusOK {
			t.Fatalf("reader plan returned %d: %s", reply.Status, reply.Body)
		}
		var plan versions.ReaderPlan
		must(t, reply.Decode(&plan))
		plan.Expires = 0
		for i := range plan.Previews {
			plan.Previews[i].URL = ""
		}
		planJSON, err := json.Marshal(plan)
		must(t, err)
		item, err := g.viewer.GetContent(t.Context(), g.ref.ID, ck.GetContentQuery{})
		must(t, err)
		var chapters []byte
		for _, v := range item.Versions {
			if v.ID == g.ref.Version {
				chapters, err = json.Marshal(v.Segments)
				must(t, err)
			}
		}
		if chapters == nil {
			t.Fatal("released edition is missing from its content")
		}
		return readerView{paths: paths, archive: g.archive(t), plan: string(planJSON), chapters: string(chapters)}
	}
	same := func(label string, got, want readerView) {
		t.Helper()
		if !slices.Equal(got.paths, want.paths) || !slices.Equal(got.archive, want.archive) || got.plan != want.plan || got.chapters != want.chapters {
			t.Fatalf("%s changed what readers see:\n got %+v\nwant %+v", label, got, want)
		}
	}
	// Only the live release keeps a copy of the release, and only in result.
	bounded := func(label string, r rowState, live string) {
		t.Helper()
		if r.live != live || r.copies != 1 || r.plans != 0 || r.total > 2 {
			var described string
			must(t, g.w.pool.QueryRow(t.Context(), `SELECT coalesce(jsonb_agg(jsonb_build_object('id',id,'state',state,'current',current,
 'plan',plan IS NOT NULL,'result',result IS NOT NULL,'error',error) ORDER BY created_at),'[]')::text FROM `+requests+`
 WHERE tenant_id=$1 AND version_id=$2`, tenant, g.ref.Version).Scan(&described))
			t.Fatalf("%s: live release %s (want %s), %d rows, %d with a result, %d with a plan: %s",
				label, r.live, live, r.total, r.copies, r.plans, described)
		}
	}

	g.readable(t, "reader/01.webp", "reader/02.webp")
	first := settle(chapterTwo)
	bounded("first release", first, first.live)
	before := view()
	for i := range 3 {
		wake()
		r := settle(chapterTwo)
		bounded("processing wake-up", r, first.live)
		if i == 0 {
			same("an unchanged re-plan", view(), before)
		}
	}
	_, err = g.admin.PostContentPublish(t.Context(), g.ref.ID, catalog.PublishInput{})
	must(t, err)
	bounded("repeated publish", settle(chapterTwo), first.live)
	same("unchanged wake-ups and publishes", view(), before)

	// The chapter date makes a real release. It supersedes the first release,
	// whose row is then removed; the new live release keeps no frozen plan.
	g.w.clock.set(chapterTwo.Add(time.Second))
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/10.webp", "reader/11.webp")
	second := settle(time.Time{})
	if second.live == first.live || second.total != 1 {
		t.Fatalf("second release kept superseded rows: %+v, first live %s", second, first.live)
	}
	bounded("second release", second, second.live)
	after := view()
	for range 2 {
		wake()
		bounded("wake-up after the last chapter", settle(time.Time{}), second.live)
	}
	same("wake-ups after the last chapter", view(), after)
}
