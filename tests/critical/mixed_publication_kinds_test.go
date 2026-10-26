package critical_test

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

func TestMediaApplicationPublishesTextKinds(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	// Configured media and unconfigured storage kinds share one application,
	// request model and worker. Posts also exercise zero and multiple attachments.
	attachMedia(t, w, a, func(cfg *ck.Config) {
		cfg.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
		post := cfg.Content.Kinds["article"]
		post.Route = "posts"
		cfg.Content.Kinds["post"] = post
		cfg.Media.Kinds = append(cfg.Media.Kinds, media.Kind{Name: "post", KeepOriginals: true,
			Uploads: []media.Upload{{Path: "images/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20, Max: 5}},
			Private: []media.Private{{Name: "reader", From: "images/{name}", To: "reader/{name}.webp", Image: &media.Image{Width: 48, Height: 48}}},
		})
	})
	admin, reader := a.engine.Client(), a.client("")
	create := func(kind, title string) catalog.Content {
		t.Helper()
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: kind, Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: title, Body: title}}}}})
		must(t, err)
		return item
	}
	article, gallery := create("article", "Text alongside media"), create("gallery", "Declared media edition")
	emptyPost, attachedPost := create("post", "Post without attachments"), create("post", "Post with two attachments")
	postRef := media.RefBody{Kind: "post", ID: attachedPost.ID, Version: attachedPost.Versions[0].ID}
	_, err := admin.IngestMedia(t.Context(), http.DefaultClient, postRef, uuid.NewString(), []ck.MediaFile{
		{Path: "images/first.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 31))},
		{Path: "images/second.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 47))},
	})
	must(t, err)
	awaitMediaReady(t, admin, postRef)
	for _, item := range []catalog.Content{article, gallery, emptyPost, attachedPost} {
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
	for _, item := range []catalog.Content{article, gallery, emptyPost, attachedPost} {
		waitForChapterPublication(t, func() bool {
			var complete bool
			must(t, w.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM `+w.schema+`.content_versions v
 JOIN `+w.schema+`.content_publication_requests r ON r.tenant_id=v.tenant_id AND r.id=v.release_id
 WHERE v.tenant_id=$1 AND v.id=$2 AND v.released AND r.state='completed')`, publicationTenant(a), item.Versions[0].ID).Scan(&complete))
			return complete
		})
	}
	// A completed SQL receipt is insufficient if the configured empty post is
	// still hidden or its body is redacted by the public reader.
	for _, post := range []catalog.Content{emptyPost, attachedPost} {
		got, err := reader.GetContent(t.Context(), post.ID, ck.GetContentQuery{})
		must(t, err)
		if !got.Live || !got.Decision.Allowed || got.Body != post.Body {
			t.Fatalf("completed post is not publicly readable: %+v", got)
		}
		edition, err := reader.GetVersion(t.Context(), post.Versions[0].ID, ck.GetVersionQuery{})
		must(t, err)
		if !edition.Live || !edition.Decision.Allowed || edition.Body != post.Body {
			t.Fatalf("completed post edition is not publicly readable: %+v", edition)
		}
	}
	view, err := reader.GetMediaVersion(t.Context(), postRef.Kind, postRef.ID, postRef.Version, ck.GetMediaVersionQuery{})
	must(t, err)
	readable := 0
	for _, file := range view.Files {
		if file.URL == "" {
			continue
		}
		response, err := http.Get(file.URL)
		must(t, err)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("published post attachment HTTP %d", response.StatusCode)
		}
		readable++
	}
	if readable != 2 {
		t.Fatalf("published post exposes %d attachments, want two", readable)
	}
	due := w.clock.now().Add(time.Hour)
	_, err = admin.PatchContentVersion(t.Context(), article.ID, article.Versions[0].ID, catalog.VersionInput{
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
 AND scheduled_at<=clock_timestamp()`, versions.QueueFor(publicationTenant(a))).Scan(&pending))
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
	var completedArticles int
	must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+w.schema+`.content_versions v
 JOIN `+w.schema+`.content_publication_requests r ON r.tenant_id=v.tenant_id AND r.id=v.release_id
 JOIN `+w.schema+`.content_items c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 WHERE v.tenant_id=$1 AND c.kind='article' AND r.state='completed'`, publicationTenant(a)).Scan(&completedArticles))
	if completedArticles != 2 {
		t.Fatalf("unified worker completed %d article editions, want two", completedArticles)
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
