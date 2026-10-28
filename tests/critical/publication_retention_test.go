package critical_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/openrails/billing"
)

// Deletion closes an edition to everyone, its buyers included: text, reader
// grants and every served file at the gateway, at once. The purchases stay,
// and a restore reopens the edition to the same grants with no new purchase.
func TestDeletedEditionClosedUntilRestore(t *testing.T) {
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
				Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Bought edition", Body: "Released text"}}}, {Languages: []string{"de"}, Text: map[string]catalog.VersionText{"en": {Title: "Bought edition", Body: "Released text"}}}},
			})
			must(t, err)
			ref := media.RefBody{Kind: kind, ID: item.ID, Version: item.Versions[0].ID}
			if kind == "gallery" {
				_, err = admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{
					{Path: "pages/01.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 17))},
				})
				must(t, err)
				awaitMediaReady(t, admin, ref)
			}
			_, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
			must(t, err)
			awaitEditionPublication(t, a, admin, ref.ID, ref.Version)
			type holder struct {
				client   *ck.Client
				customer billing.CustomerID
				key      string
			}
			grant := func(key access.Key, keyErr error) holder {
				must(t, keyErr)
				person, credential := w.person(t, 0)
				customer, err := billing.ParseCustomerID(person.ID)
				must(t, err)
				_, err = b.merchant.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
				must(t, err)
				product, err := b.merchant.CreateProduct(t.Context(), billing.CreateProductParams{Key: uuid.NewString(), DisplayName: "Bought edition", Entitlements: []string{key.String()}})
				must(t, err)
				_, err = b.merchant.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: uuid.NewString(), Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID}}})
				must(t, err)
				return holder{a.client(credential), customer, key.String()}
			}
			exact := grant(access.VersionKey(a.cfg.Scope.AppID.String(), ref.ID, ref.Version))
			parent := grant(access.ContentKey(a.cfg.Scope.AppID.String(), ref.ID))
			status := func(address string) int {
				t.Helper()
				response, err := http.Get(address)
				must(t, err)
				response.Body.Close()
				return response.StatusCode
			}
			read := func(h holder) string {
				t.Helper()
				got, err := h.client.GetVersion(t.Context(), ref.Version, ck.GetVersionQuery{})
				must(t, err)
				if !got.Decision.Allowed || got.Body != "Released text" {
					t.Fatalf("buyer cannot read the live edition: %+v", got)
				}
				if kind != "gallery" {
					return ""
				}
				files, err := h.client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
				must(t, err)
				if len(files.Files) != 1 || files.Files[0].Path != "reader/01.webp" || files.Files[0].URL == "" {
					t.Fatalf("buyer lacks the released page: %+v", files)
				}
				if code := status(files.Files[0].URL); code != http.StatusOK {
					t.Fatalf("released page returned HTTP %d", code)
				}
				return files.Files[0].URL
			}
			page := read(exact)
			must(t, admin.DeleteContentVersion(t.Context(), ref.ID, ref.Version))
			closed := func() {
				t.Helper()
				for _, reader := range []*ck.Client{exact.client, parent.client, a.client("")} {
					_, err := reader.GetVersion(t.Context(), ref.Version, ck.GetVersionQuery{})
					requireError(t, err, 404, "not_found")
				}
				if kind == "gallery" {
					_, err := exact.client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
					requireError(t, err, 404, "not_found")
					if code := status(page); code != http.StatusNotFound {
						t.Fatalf("deleted edition's page URL returned HTTP %d to its buyer", code)
					}
				}
			}
			closed()
			awaitPublicationJobsIdle(t, w, a)
			closed()
			// Deletion changes availability only: both purchases still hold.
			for _, h := range []holder{exact, parent} {
				rights, err := b.merchant.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: h.customer, Entitlements: []string{h.key}})
				must(t, err)
				if !rights.Entitlements[h.key] {
					t.Fatalf("deletion revoked a purchase: %s", h.key)
				}
			}
			_, err = admin.PatchContentVersion(t.Context(), ref.ID, ref.Version, catalog.VersionInput{State: "published"})
			must(t, err)
			awaitEditionPublication(t, a, admin, ref.ID, ref.Version)
			read(exact)
		})
	}
}

// A deleted version's HLS files stay where they are behind the fence; the
// restore reopens the same immutable segment URL.
func TestDeletedHLSClosedUntilRestore(t *testing.T) {
	f := newTinyHLSFixture(t)
	admin := f.app.engine.Client()
	checkURL := func(address string, want int) {
		t.Helper()
		response, err := http.Get(address)
		must(t, err)
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("HLS origin returned %d, want %d", response.StatusCode, want)
		}
	}
	checkURL(f.SegmentURL, http.StatusOK)
	must(t, admin.DeleteContentVersion(t.Context(), f.Ref.ID, f.Ref.Version))
	checkURL(f.SegmentURL, http.StatusNotFound)
	awaitPublicationJobsIdle(t, f.world, f.app)
	checkURL(f.SegmentURL, http.StatusNotFound)
	_, err := f.Reader.GetMediaVersionHls(t.Context(), f.Ref.Kind, f.Ref.ID, f.Ref.Version, f.ChildPath, ck.GetMediaVersionHlsQuery{})
	requireError(t, err, 404, "not_found")
	_, err = admin.PatchContentVersion(t.Context(), f.Ref.ID, f.Ref.Version, catalog.VersionInput{State: "published"})
	must(t, err)
	checkURL(f.SegmentURL, http.StatusOK)
	awaitPublicationJobsIdle(t, f.world, f.app)
	child, err := f.Reader.GetMediaVersionHls(t.Context(), f.Ref.Kind, f.Ref.ID, f.Ref.Version, f.ChildPath, ck.GetMediaVersionHlsQuery{})
	must(t, err)
	if restored := playlistURIForTest(t, child.Body); restored != f.SegmentURL {
		t.Fatalf("restore moved the served segment: %s, was %s", restored, f.SegmentURL)
	}
	checkURL(f.SegmentURL, http.StatusOK)
}
