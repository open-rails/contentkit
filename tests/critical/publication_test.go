package critical_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
)

func TestPublicationWithoutWorkers(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	admin, reader := a.engine.Client(), a.client("")
	base := w.clock.now()
	newDraft := func(t *testing.T, rule *access.Rule) catalog.Content {
		t.Helper()
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Access: rule,
			Text: map[string]catalog.Text{"en": {Title: "Scheduled article", Body: "Embargoed manuscript"}}})
		must(t, err)
		return item
	}
	t.Run("CK-J05-S01_immediate_publication_is_visible", func(t *testing.T) {
		w.clock.set(base)
		item := newDraft(t, nil)
		_, err := reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		published, err := admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		if !published.Live {
			t.Fatal("immediate publication did not become live")
		}
		got, err := reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if !got.Decision.Allowed || got.Body != "Embargoed manuscript" {
			t.Fatalf("published body was not readable: %+v", got)
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
	t.Run("CK-J05-S02_scheduled_content_opens_at_deadline_without_recorder", func(t *testing.T) {
		w.clock.set(base)
		item := newDraft(t, nil)
		at := base.Add(time.Hour)
		_, err := admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{PublishAt: &at})
		must(t, err)
		w.clock.set(at.Add(-time.Microsecond))
		_, err = reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		w.clock.set(at)
		got, err := reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if !got.Live || !got.Decision.Allowed || got.Body != "Embargoed manuscript" {
			t.Fatalf("runtime deadline did not open content: %+v", got)
		}
		page, err := reader.GetContents(t.Context(), ck.GetContentsQuery{})
		must(t, err)
		found := false
		for _, card := range page.Contents {
			if card.ID == item.ID {
				found = true
			}
		}
		if !found {
			t.Fatal("runtime deadline opened detail but not listing")
		}
	})
	t.Run("CK-J05-S04_public_audience_release_opens_without_a_grant", func(t *testing.T) {
		w.clock.set(base)
		at := base.Add(time.Hour)
		item := newDraft(t, &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}, Releases: []access.Release{{At: at, Level: access.Public}}})
		_, err := admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		w.clock.set(at.Add(-time.Microsecond))
		locked, err := reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if locked.Decision.Allowed || locked.Body != "" {
			t.Fatalf("future audience leaked the body: %+v", locked)
		}
		w.clock.set(at)
		open, err := reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if !open.Decision.Allowed || open.Body != "Embargoed manuscript" {
			t.Fatalf("due public audience remains locked: %+v", open)
		}
	})
	t.Run("CK-J05-S05_unpublish_and_reschedule_remove_public_visibility", func(t *testing.T) {
		w.clock.set(base)
		item := newDraft(t, nil)
		_, err := admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		_, err = admin.PostContentUnpublish(t.Context(), item.ID)
		must(t, err)
		_, err = reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		at := base.Add(2 * time.Hour)
		_, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{PublishAt: &at})
		must(t, err)
		_, err = reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		w.clock.set(at)
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
		item, err := engine.Client().PostContents(t.Context(), catalog.ContentInput{Kind: "article", Access: rule, Text: map[string]catalog.Text{"en": {Title: "Wall clock", Body: "Wall-clock body"}}})
		must(t, err)
		_, err = engine.Client().PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		locked, err := anonymous.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if locked.Decision.Allowed || locked.Body != "" {
			t.Fatal("nil clock opened a future audience")
		}
		rule.Releases[0].At = past
		_, err = engine.Client().PatchContent(t.Context(), item.ID, catalog.ContentPatch{Access: rule})
		must(t, err)
		open, err := anonymous.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if !open.Decision.Allowed || open.Body != "Wall-clock body" {
			t.Fatal("nil clock failed to open the already-released audience")
		}
	})
}
