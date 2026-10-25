package critical_test

import (
	"bytes"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/openrails/billing"
)

func TestDeletedEditionRetainsCompletedPublication(t *testing.T) {
	for _, test := range []struct {
		name, kind string
		level      access.Level
	}{
		{"article", "article", access.PPV},
		{"private_gallery", "gallery", access.PPV},
		{"public_gallery", "gallery", access.Public},
	} {
		t.Run(test.name, func(t *testing.T) {
			kind := test.kind
			w := newWorld(t, func(c *ck.Config) {
				k := c.Content.Kinds["article"]
				k.Versions.Hidden = false
				c.Content.Kinds["article"] = k
			})
			a := w.apps[0]
			b := attachBilling(t, w, a)
			if kind == "gallery" {
				attachMedia(t, w, a, func(c *ck.Config) {
					c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
				})
			} else {
				startPublicationRuntime(t, a)
			}
			admin := a.engine.Client()
			item, err := admin.PostContents(t.Context(), catalog.ContentInput{
				Kind: kind, Access: &access.Rule{Level: test.level},
				Text:     map[string]catalog.Text{"en": {Title: "Retained purchase", Body: "Previously released text"}},
				Versions: []catalog.VersionInput{{Languages: []string{"en"}}, {Languages: []string{"de"}}},
			})
			must(t, err)
			ref := media.RefBody{Kind: kind, ID: item.ID, Version: item.Versions[0].ID}
			future := w.clock.now().Add(time.Hour)
			if kind == "gallery" {
				_, err = admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{
					{Path: "pages/01.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 17))},
					{Path: "pages/20.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 83))},
				})
				must(t, err)
				awaitMediaReady(t, admin, ref)
				_, err = admin.PutContentVersionSegments(t.Context(), ref.ID, ref.Version, catalog.SegmentsInput{Segments: []catalog.SegmentInput{
					{Position: 0, StartPage: "pages/01.png"},
					{Position: 1, StartPage: "pages/20.png", ReleaseAt: &future},
				}})
				must(t, err)
			}
			_, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
			must(t, err)
			awaitEditionPublication(t, a, admin, ref.ID, ref.Version)
			grant := func(key access.Key, keyErr error) *ck.Client {
				must(t, keyErr)
				person, credential := w.person(t, 0)
				customer, err := billing.ParseCustomerID(person.ID)
				must(t, err)
				_, err = b.merchant.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
				must(t, err)
				product, err := b.merchant.CreateProduct(t.Context(), billing.CreateProductParams{Key: uuid.NewString(), DisplayName: "Retained access", Entitlements: []string{key.String()}})
				must(t, err)
				_, err = b.merchant.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: uuid.NewString(), Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID}}})
				must(t, err)
				return a.client(credential)
			}
			holder := grant(access.VersionKey(a.cfg.Scope.AppID.String(), ref.ID, ref.Version))
			parentHolder := grant(access.ContentKey(a.cfg.Scope.AppID.String(), ref.ID))
			read := func() string {
				t.Helper()
				got, err := holder.GetVersion(t.Context(), ref.Version, ck.GetVersionQuery{})
				must(t, err)
				if !got.Decision.Allowed || got.Body != "Previously released text" {
					t.Fatalf("exact edition holder lost retained text: %+v", got)
				}
				if kind == "gallery" {
					got, err := holder.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
					must(t, err)
					if len(got.Files) != 1 || got.Files[0].Path != "reader/01.webp" || got.Files[0].URL == "" {
						t.Fatalf("retention changed the completed chapter set: %+v", got)
					}
					response, err := http.Get(got.Files[0].URL)
					must(t, err)
					response.Body.Close()
					if response.StatusCode != http.StatusOK {
						t.Fatalf("retained media URL returned HTTP %d", response.StatusCode)
					}
					return got.Files[0].URL
				}
				return ""
			}
			originalURL := read()
			sibling, err := holder.GetVersion(t.Context(), item.Versions[1].ID, ck.GetVersionQuery{})
			must(t, err)
			if test.level != access.Public && sibling.Decision.Allowed {
				t.Fatal("edition grant opened a sibling")
			}
			must(t, admin.DeleteContentVersion(t.Context(), ref.ID, ref.Version))
			awaitPublicationJobsIdle(t, w, a)
			retainedURL := read()
			if test.level == access.Public {
				before, err := url.Parse(originalURL)
				must(t, err)
				after, err := url.Parse(retainedURL)
				must(t, err)
				if before.RawQuery != "" || after.Query().Get("t") == "" {
					t.Fatal("formerly public reader bytes did not become private")
				}
				response, err := http.Get(originalURL)
				must(t, err)
				response.Body.Close()
				if response.StatusCode != http.StatusNotFound {
					t.Fatalf("deleted public origin still bypasses paid retention: HTTP %d", response.StatusCode)
				}
			}
			for _, reader := range []*ck.Client{a.client(""), parentHolder} {
				_, err := reader.GetVersion(t.Context(), ref.Version, ck.GetVersionQuery{})
				requireError(t, err, 404, "not_found")
			}
			// Deleting the sibling also deletes the parent, but must not revoke
			// this independently purchased edition's completed publication.
			must(t, admin.DeleteContentVersion(t.Context(), ref.ID, item.Versions[1].ID))
			awaitPublicationJobsIdle(t, w, a)
			read()
			_, err = holder.GetVersion(t.Context(), item.Versions[1].ID, ck.GetVersionQuery{})
			requireError(t, err, 404, "not_found")
			// Scheduled chapters remain frozen after deletion even when their
			// deadline passes and real durable workers process every due job.
			w.clock.set(future.Add(time.Hour))
			awaitPublicationJobsIdle(t, w, a)
			read()
		})
	}
}

// Exact-key retention is covered above; this uses an editor's ordinary reader
// projection to isolate actual immutable HLS index remapping across both moves.
func TestDeletedHLSRetainsImmutableIndex(t *testing.T) {
	f := newTinyHLSFixture(t)
	admin := f.app.engine.Client()
	must(t, admin.DeleteContentVersion(t.Context(), f.Ref.ID, f.Ref.Version))
	awaitPublicationJobsIdle(t, f.world, f.app)
	child, err := admin.GetMediaVersionHls(t.Context(), f.Ref.Kind, f.Ref.ID, f.Ref.Version, f.ChildPath, ck.GetMediaVersionHlsQuery{})
	must(t, err)
	retained := playlistURIForTest(t, child.Body)
	address, err := url.Parse(retained)
	must(t, err)
	if address.Query().Get("t") == "" {
		t.Fatal("retained HLS segment is not private")
	}
	checkURL := func(address string, status int) {
		t.Helper()
		response, err := http.Get(address)
		must(t, err)
		response.Body.Close()
		if response.StatusCode != status {
			t.Fatalf("HLS origin returned %d, want %d", response.StatusCode, status)
		}
	}
	checkURL(retained, http.StatusOK)
	checkURL(f.SegmentURL, http.StatusNotFound)
	address.RawQuery = ""
	checkURL(address.String(), http.StatusNotFound)
	_, err = f.Reader.GetMediaVersionHls(t.Context(), f.Ref.Kind, f.Ref.ID, f.Ref.Version, f.ChildPath, ck.GetMediaVersionHlsQuery{})
	requireError(t, err, 404, "not_found")
	// Restore must use the original authored public policy and keep the
	// immutable index playable through its original preparation references.
	_, err = admin.PatchContentVersion(t.Context(), f.Ref.ID, f.Ref.Version, catalog.VersionInput{State: "published"})
	must(t, err)
	awaitPublicationJobsIdle(t, f.world, f.app)
	child, err = f.Reader.GetMediaVersionHls(t.Context(), f.Ref.Kind, f.Ref.ID, f.Ref.Version, f.ChildPath, ck.GetMediaVersionHlsQuery{})
	must(t, err)
	restored := playlistURIForTest(t, child.Body)
	address, err = url.Parse(restored)
	must(t, err)
	if address.RawQuery != "" {
		t.Fatal("restore replaced the authored public policy")
	}
	checkURL(restored, http.StatusOK)
	checkURL(retained, http.StatusNotFound)
}
