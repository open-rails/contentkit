package critical_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	versionstore "github.com/open-rails/contentkit/internal/versionstore"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/openrails/billing"
)

func TestScheduledPolicyPublishesWithoutStorageAreaChange(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	b := attachBilling(t, w, a)
	attachMedia(t, w, a, func(cfg *ck.Config) {
		cfg.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	admin := a.engine.Client()
	user, credential := w.person(t, 0)
	customer, err := billing.ParseCustomerID(user.ID)
	must(t, err)
	_, err = b.merchant.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
	must(t, err)
	gold, err := b.merchant.CreateProduct(t.Context(), billing.CreateProductParams{Key: "scheduled-policy-gold", DisplayName: "Gold", Entitlements: []string{"gold"}})
	must(t, err)
	_, err = b.merchant.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: gold.ID}}})
	must(t, err)
	due := w.clock.now().Add(time.Hour)
	create := func(title string, release access.Release) catalog.Content {
		t.Helper()
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "gallery",
			Access: &access.Rule{Level: access.PPV, Releases: []access.Release{release}}, Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: title, Body: "Released policy body"}}}},
		})
		must(t, err)
		return item
	}
	private := create("Same private placement", access.Release{At: due, Level: access.TierLevel, Tiers: []string{"gold"}})
	empty := create("Media-bound text without copies", access.Release{At: due, Level: access.Public})
	ref := media.RefBody{Kind: "gallery", ID: private.ID, Version: private.Versions[0].ID}
	_, err = admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/01.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 73))}})
	must(t, err)
	awaitMediaReady(t, admin, ref)
	for _, item := range []catalog.Content{private, empty} {
		_, err := admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
	}
	type snapshot struct {
		id     string
		access access.Rule
		copies int
	}
	read := func(version string) snapshot {
		t.Helper()
		var out snapshot
		var raw []byte
		must(t, w.pool.QueryRow(t.Context(), `SELECT coalesce(v.release_id::text,''),v.released_access,
 coalesce(jsonb_array_length(nullif(r.result->'copies','null'::jsonb)),0)
 FROM `+w.schema+`.content_versions v LEFT JOIN `+w.schema+`.content_publication_requests r
 ON r.tenant_id=v.tenant_id AND r.id=v.release_id
 WHERE v.tenant_id=$1 AND v.id=$2`, a.cfg.Scope.AppID.String(), version).Scan(&out.id, &raw, &out.copies))
		if len(raw) > 0 {
			must(t, json.Unmarshal(raw, &out.access))
		}
		return out
	}
	waitForChapterPublication(t, func() bool { return read(private.Versions[0].ID).id != "" && read(empty.Versions[0].ID).id != "" })
	// Drain immediate follow-ups before advancing the fixture clock: a worker
	// already copying would legitimately observe a newly mature policy.
	waitForChapterPublication(t, func() bool {
		var count int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+w.schema+`_river.river_job
 WHERE queue=$1 AND state IN ('available','pending','running','retryable','scheduled')
 AND scheduled_at<=clock_timestamp()`, versionstore.QueueFor(a.cfg.Scope.AppID.String())).Scan(&count))
		return count == 0
	})
	initialPrivate, initialEmpty := read(private.Versions[0].ID), read(empty.Versions[0].ID)
	if len(initialPrivate.access.Releases) != 0 || len(initialEmpty.access.Releases) != 0 || initialEmpty.copies != 0 {
		t.Fatal("initial completed policy included a future audience or fabricated storage work")
	}
	goldReader, anonymous := a.client(credential), a.client("")
	check := func(client *ck.Client, item catalog.Content, allowed bool) {
		t.Helper()
		got, err := client.GetVersion(t.Context(), item.Versions[0].ID, ck.GetVersionQuery{})
		must(t, err)
		if got.Decision.Allowed != allowed || allowed && got.Body != "Released policy body" || !allowed && got.Body != "" {
			t.Fatalf("completed audience access=%v, want %v: %+v", got.Decision.Allowed, allowed, got.Decision)
		}
	}
	check(goldReader, private, false)
	check(anonymous, empty, false)
	w.clock.set(due.Add(time.Second))
	b.clock.Advance(w.clock.now().Sub(b.clock.Now()))
	check(goldReader, private, false)
	check(anonymous, empty, false)
	waitForChapterPublication(t, func() bool {
		wakeDuePublicationJobs(t, a)
		p, e := read(private.Versions[0].ID), read(empty.Versions[0].ID)
		return p.id != initialPrivate.id && e.id != initialEmpty.id && len(p.access.Releases) == 1 && len(e.access.Releases) == 1
	})
	check(goldReader, private, true)
	check(anonymous, private, false)
	check(anonymous, empty, true)
	if read(empty.Versions[0].ID).copies != 0 {
		t.Fatal("media-bound empty edition needed no storage copies")
	}
	view, err := goldReader.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
	must(t, err)
	for _, file := range view.Files {
		if file.Path == "reader/01.webp" && file.URL != "" {
			if !strings.Contains(file.URL, "/private/") {
				t.Fatalf("tier audience changed private placement: %s", file.URL)
			}
			return
		}
	}
	t.Fatal("new tier audience cannot read its completed private edition")
}
