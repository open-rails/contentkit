package critical_test

import (
	"testing"
	"time"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
)

func TestMediaApplicationPublishesTextKinds(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	// Only galleries declare media. Articles must retain the text worker even
	// though this same application has an active storage publication service.
	attachMedia(t, w, a, func(cfg *ck.Config) {
		cfg.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	admin, reader := a.engine.Client(), a.client("")
	create := func(kind, title string) catalog.Content {
		t.Helper()
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: kind, Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: title, Body: title}}}}})
		must(t, err)
		return item
	}
	article, gallery := create("article", "Text alongside media"), create("gallery", "Declared media edition")
	for _, item := range []catalog.Content{article, gallery} {
		_, err := admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
	}
	waitForChapterPublication(t, func() bool {
		got, err := reader.GetContent(t.Context(), article.ID, ck.GetContentQuery{})
		return err == nil && got.Live && got.Body == "Text alongside media"
	})
	waitForChapterPublication(t, func() bool {
		var ready bool
		must(t, w.pool.QueryRow(t.Context(), `SELECT released AND release_id IS NOT NULL FROM `+w.schema+`.content_versions
 WHERE tenant_id=$1 AND id=$2`, a.cfg.Scope.AppID.String(), gallery.Versions[0].ID).Scan(&ready))
		return ready
	})
	due := w.clock.now().Add(time.Hour)
	_, err := admin.PatchContentVersion(t.Context(), article.ID, article.Versions[0].ID, catalog.VersionInput{
		Access: &access.Rule{Level: access.PPV, Releases: []access.Release{{At: due, Level: access.Public}}},
	})
	must(t, err)
	waitForChapterPublication(t, func() bool {
		got, err := reader.GetContent(t.Context(), article.ID, ck.GetContentQuery{})
		return err == nil && got.Live && !got.Decision.Allowed && got.Body == ""
	})
	scheduled := create("article", "Scheduled text alongside media")
	_, err = admin.PostContentPublish(t.Context(), scheduled.ID, catalog.PublishInput{PublishAt: &due})
	must(t, err)
	waitForChapterPublication(t, func() bool {
		var pending int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+w.schema+`_river.river_job
 WHERE queue=$1 AND state IN ('available','pending','running','retryable','scheduled')
 AND scheduled_at<=clock_timestamp()`, catalog.QueueFor(a.cfg.Scope.AppID.String())).Scan(&pending))
		return pending == 0
	})
	w.clock.set(due.Add(time.Second))
	_, err = reader.GetContent(t.Context(), scheduled.ID, ck.GetContentQuery{})
	requireError(t, err, 404, "not_found")
	locked, err := reader.GetContent(t.Context(), article.ID, ck.GetContentQuery{})
	must(t, err)
	if locked.Decision.Allowed {
		t.Fatal("text audience opened from the clock without its worker")
	}
	waitForChapterPublication(t, func() bool {
		wakeDuePublicationJobs(t, a)
		first, err := reader.GetContent(t.Context(), article.ID, ck.GetContentQuery{})
		if err != nil || !first.Decision.Allowed {
			return false
		}
		second, err := reader.GetContent(t.Context(), scheduled.ID, ck.GetContentQuery{})
		return err == nil && second.Live && second.Body == "Scheduled text alongside media"
	})
	var wrongRequests int
	must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+w.schema+`.content_publication_requests r
 JOIN `+w.schema+`.content_items c ON c.tenant_id=r.tenant_id AND c.id=r.content_id
 WHERE r.tenant_id=$1 AND c.kind='article'`, a.cfg.Scope.AppID.String()).Scan(&wrongRequests))
	if wrongRequests != 0 {
		t.Fatalf("nonmedia articles created %d storage requests", wrongRequests)
	}
	_, err = admin.PostContentUnpublish(t.Context(), article.ID)
	must(t, err)
	waitForChapterPublication(t, func() bool {
		wakeDuePublicationJobs(t, a)
		var released bool
		must(t, w.pool.QueryRow(t.Context(), `SELECT released FROM `+w.schema+`.content_versions WHERE tenant_id=$1 AND id=$2`,
			a.cfg.Scope.AppID.String(), article.Versions[0].ID).Scan(&released))
		return !released
	})
}
