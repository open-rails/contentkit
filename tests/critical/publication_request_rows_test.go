package critical_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/jobhook"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	versionstore "github.com/open-rails/contentkit/internal/versionstore"
	versions "github.com/open-rails/contentkit/media/versions"
	"github.com/riverqueue/river"
)

// Processing wake-ups and repeated publishes of a released edition re-plan it.
// A run that changes nothing must not add a request row or copy the live
// release; a real release removes the finished requests it supersedes.
func TestUnchangedReleaseLeavesNoRequestCopies(t *testing.T) {
	w := newChapterArchiveWorld(t)
	requests := pgx.Identifier{w.schema, "content_publication_requests"}.Sanitize()
	// The pruning deletes a no-op row soon after it completes, so record every
	// request that ever had a release written into its result.
	writes, record := pgx.Identifier{w.schema, "critical_release_writes"}.Sanitize(), pgx.Identifier{w.schema, "critical_record_release"}.Sanitize()
	_, err := w.pool.Exec(t.Context(), `CREATE TABLE `+writes+` (id uuid NOT NULL);
CREATE FUNCTION `+record+`() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.result IS NOT NULL THEN INSERT INTO `+writes+` VALUES (NEW.id); END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER critical_record_release AFTER INSERT OR UPDATE OF result ON `+requests+`
 FOR EACH ROW EXECUTE FUNCTION `+record+`()`)
	must(t, err)
	written := func() []string {
		t.Helper()
		var ids []string
		must(t, w.pool.QueryRow(t.Context(), `SELECT coalesce(array_agg(DISTINCT id::text ORDER BY id::text),'{}') FROM `+writes).Scan(&ids))
		return ids
	}
	g := newHiddenStartGallery(t, w, false, []string{"pages/01.png", "pages/02.png", "pages/10.png", "pages/11.png"}, laterChapters(time.Hour))
	chapterTwo := g.start.Add(time.Hour)
	tenant := publicationTenant(g.a)

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
	// releases lists every request ever given a result, live or since pruned.
	bounded := func(label string, r rowState, live string, releases ...string) {
		t.Helper()
		slices.Sort(releases)
		if got := written(); !slices.Equal(got, releases) {
			t.Fatalf("%s: releases were written into requests %v, want only %v", label, got, releases)
		}
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
	// Setup may complete more than one real release; none may come later.
	base := written()
	if !slices.Contains(base, first.live) {
		t.Fatalf("live release %s was never written into its request: %v", first.live, base)
	}
	bounded("first release", first, first.live, base...)
	before := view()
	for i := range 3 {
		wake()
		r := settle(chapterTwo)
		bounded("processing wake-up", r, first.live, base...)
		if i == 0 {
			same("an unchanged re-plan", view(), before)
		}
	}
	_, err = g.admin.PostContentPublish(t.Context(), g.ref.ID, catalog.PublishInput{})
	must(t, err)
	bounded("repeated publish", settle(chapterTwo), first.live, base...)
	same("unchanged wake-ups and publishes", view(), before)

	// The chapter date makes a real release. It supersedes the first release,
	// whose row is then removed; the new live release keeps no frozen plan.
	g.w.clock.set(chapterTwo.Add(time.Second))
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/10.webp", "reader/11.webp")
	second := settle(time.Time{})
	if second.live == first.live || second.total != 1 {
		t.Fatalf("second release kept superseded rows: %+v, first live %s", second, first.live)
	}
	bounded("second release", second, second.live, append(slices.Clone(base), second.live)...)
	after := view()
	for range 2 {
		wake()
		bounded("wake-up after the last chapter", settle(time.Time{}), second.live, append(slices.Clone(base), second.live)...)
	}
	same("wake-ups after the last chapter", view(), after)
}

// A withdrawn edition keeps its release id but no longer keeps a release plan.
// Publishing it again for a later date still reconciles at once, as it did
// when completed releases kept their plans.
func TestWithdrawnEditionRepublishesAtOnce(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	fleet, err := jobhook.Fleet(t.Context(), a.engine, &river.Config{Schema: publicationRiverSchema(a), Queues: map[string]river.QueueConfig{}})
	must(t, err)
	t.Cleanup(func() { must(t, fleet.Stop(context.Background())) })
	item, err := a.engine.Client().PostContents(t.Context(), catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{{Languages: []string{"en"},
		Text: map[string]catalog.VersionText{"en": {Title: "Withdrawn and published again", Body: "No storage service is configured."}}}}})
	must(t, err)
	version := item.Versions[0].ID
	_, err = a.engine.Client().PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	runPublicationWorker(t, fleet, a)
	_, err = a.engine.Client().PostContentUnpublish(t.Context(), item.ID)
	must(t, err)
	runPublicationWorker(t, fleet, a)
	var released bool
	var plans int
	must(t, w.pool.QueryRow(t.Context(), `SELECT v.released,(SELECT count(*) FROM `+w.schema+`.content_publication_requests r
 WHERE r.tenant_id=v.tenant_id AND r.version_id=v.id AND r.plan IS NOT NULL)
 FROM `+w.schema+`.content_versions v WHERE v.tenant_id=$1 AND v.id=$2 AND v.release_id IS NOT NULL`, publicationTenant(a), version).Scan(&released, &plans))
	if released || plans != 0 {
		t.Fatalf("withdrawal left released=%v and %d release plans", released, plans)
	}
	later := w.clock.now().Add(time.Hour)
	_, err = a.engine.Client().PostContentPublish(t.Context(), item.ID, catalog.PublishInput{PublishAt: &later})
	must(t, err)
	if due := currentPublicationJob(t, a, version).Due; !due.Equal(w.clock.now()) {
		t.Fatalf("republishing a withdrawn edition is due %v, want at once (%v)", due, w.clock.now())
	}
}
