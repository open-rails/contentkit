package critical_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/jobhook"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/riverqueue/river"
)

// runPublicationWorker executes the real registered durable worker, then stops
// it so the next clock-only assertion cannot be satisfied by a running worker.
func runPublicationWorker(t *testing.T, fleet *river.Client[pgx.Tx], app *application) {
	t.Helper()
	wakeDuePublicationJobs(t, app)
	must(t, fleet.Start(t.Context()))
	awaitDuePublications(t, app)
	must(t, fleet.Stop(t.Context()))
}

func TestPublicationWithoutWorkers(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	admin, reader := a.engine.Client(), a.client("")
	fleet, err := jobhook.Fleet(t.Context(), a.engine, &river.Config{Schema: w.schema + "_river", Queues: map[string]river.QueueConfig{}})
	must(t, err)
	t.Cleanup(func() { must(t, fleet.Stop(context.Background())) })
	base := w.clock.now()
	newDraft := func(t *testing.T, rule *access.Rule) catalog.Content {
		t.Helper()
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Access: rule, Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Scheduled article", Body: "Embargoed manuscript"}}}}})
		must(t, err)
		return item
	}
	t.Run("CK-J05-S01_immediate_publication_is_visible", func(t *testing.T) {
		w.clock.set(base)
		item := newDraft(t, nil)
		_, err := reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		accepted, err := admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		if accepted.Live {
			t.Fatal("acceptance was confused with completed publication")
		}
		_, err = reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		runPublicationWorker(t, fleet, a)
		got, err := reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if !got.Decision.Allowed || got.Body != "Embargoed manuscript" {
			t.Fatalf("completed body was not readable: %+v", got)
		}
		log, err := admin.ListEvents(t.Context(), catalog.EventQuery{Types: []string{"content.published"}})
		must(t, err)
		n := 0
		for _, event := range log.Events {
			if event.ContentID == item.ID {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("publication produced %d content.published facts; want one", n)
		}
	})
	// Keep the historical scenario ID/name: its acceptance now proves that a
	// date alone cannot grant access, followed by completed real worker access.
	t.Run("CK-J05-S02_scheduled_content_opens_at_deadline_without_recorder", func(t *testing.T) {
		w.clock.set(base)
		item := newDraft(t, nil)
		at := base.Add(time.Hour)
		_, err := admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{PublishAt: &at})
		must(t, err)
		for _, now := range []time.Time{at.Add(-time.Microsecond), at, at.Add(6 * time.Hour)} {
			w.clock.set(now)
			_, err = reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
			requireError(t, err, 404, "not_found")
		}
		runPublicationWorker(t, fleet, a)
		got, err := reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if !got.Live || !got.Decision.Allowed || got.Body != "Embargoed manuscript" {
			t.Fatalf("completed release did not open content: %+v", got)
		}
		page, err := reader.GetContents(t.Context(), ck.GetContentsQuery{})
		must(t, err)
		found := false
		for _, card := range page.Contents {
			found = found || card.ID == item.ID
		}
		if !found {
			t.Fatal("completed detail was absent from discovery")
		}
	})
	t.Run("CK-J05-S04_public_audience_release_opens_without_a_grant", func(t *testing.T) {
		w.clock.set(base)
		at := base.Add(time.Hour)
		item := newDraft(t, &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}, Releases: []access.Release{{At: at, Level: access.Public}}})
		_, err := admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		runPublicationWorker(t, fleet, a)
		for _, now := range []time.Time{at.Add(-time.Microsecond), at} {
			w.clock.set(now)
			locked, err := reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
			must(t, err)
			if locked.Decision.Allowed || locked.Body != "" {
				t.Fatal("clock opened an uncompleted audience promotion")
			}
		}
		runPublicationWorker(t, fleet, a)
		open, err := reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if !open.Decision.Allowed || open.Body != "Embargoed manuscript" {
			t.Fatalf("completed public audience remains locked: %+v", open)
		}
	})
	t.Run("CK-J05-S05_unpublish_and_reschedule_remove_public_visibility", func(t *testing.T) {
		w.clock.set(base)
		item := newDraft(t, nil)
		_, err := admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		runPublicationWorker(t, fleet, a)
		_, err = admin.PostContentUnpublish(t.Context(), item.ID)
		must(t, err)
		_, err = reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		runPublicationWorker(t, fleet, a)
		at := base.Add(2 * time.Hour)
		_, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{PublishAt: &at})
		must(t, err)
		for _, now := range []time.Time{base, at} {
			w.clock.set(now)
			_, err = reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
			requireError(t, err, 404, "not_found")
		}
		runPublicationWorker(t, fleet, a)
		_, err = reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
	})
	t.Run("default_clock_retains_wall_time_audience_behavior", func(t *testing.T) {
		cfg, deps := a.cfg, a.deps
		cfg.Database.SkipMigrations = true
		deps.Clock = nil
		engine, err := ck.New(t.Context(), cfg, deps)
		must(t, err)
		defer func() { must(t, engine.Close(context.Background())) }()
		liveFleet, err := jobhook.Fleet(t.Context(), engine, &river.Config{Schema: w.schema + "_river", Queues: map[string]river.QueueConfig{}})
		must(t, err)
		defer func() { must(t, liveFleet.Stop(context.Background())) }()
		routes, err := engine.Routes(ck.Routes{Groups: ck.ContentRoutes})
		must(t, err)
		mux := http.NewServeMux()
		for _, route := range routes {
			mux.Handle(route.Path, route.Handler)
		}
		srv := httptest.NewServer(mux)
		defer srv.Close()
		anonymous := ck.NewClient(srv.URL, nil)
		future, past := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
		rule := &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}, Releases: []access.Release{{At: future, Level: access.Public}}}
		item, err := engine.Client().PostContents(t.Context(), catalog.ContentInput{Kind: "article", Access: rule, Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Wall clock", Body: "Wall-clock body"}}}}})
		must(t, err)
		_, err = engine.Client().PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		runPublicationWorker(t, liveFleet, &application{cfg: cfg, deps: deps})
		locked, err := anonymous.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if locked.Decision.Allowed || locked.Body != "" {
			t.Fatal("nil clock opened a future audience")
		}
		rule.Releases[0].At = past
		_, err = engine.Client().PatchContentVersion(t.Context(), item.ID, item.Versions[0].ID, catalog.VersionInput{Access: rule})
		must(t, err)
		locked, err = anonymous.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if locked.Decision.Allowed || locked.Body != "" {
			t.Fatal("policy edit bypassed durable audience publication")
		}
		runPublicationWorker(t, liveFleet, &application{cfg: cfg, deps: deps})
		open, err := anonymous.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if !open.Decision.Allowed || open.Body != "Wall-clock body" {
			t.Fatal("completed wall-clock audience did not open")
		}
	})
}
