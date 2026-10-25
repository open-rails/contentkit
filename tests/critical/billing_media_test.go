package critical_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/openrails/billing"
)

func TestCommerceMediaGrantRevocation(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	b := attachBilling(t, w, a)
	attachMedia(t, w, a, func(c *ck.Config) {
		kind := &c.Media.Config.Kinds[0]
		kind.Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}, Download: "archive"}
		kind.Private = append(kind.Private,
			media.Private{Name: "archive-pages", From: "pages/{name}", To: "download-pages/{name}{ext}", HostOnly: true, Image: &media.Image{Format: media.ImageFormatSource}},
			media.Private{Name: "archive", To: "pages.zip", Zip: "download-pages/", HostOnly: true})
	})
	admin := a.engine.Client()
	ref := galleryRef(t, admin, "Paid gallery")
	_, err := admin.PatchContent(t.Context(), ref.ID, catalog.ContentPatch{Access: &access.Rule{Level: access.PPV}})
	must(t, err)
	payload := imageBytes(t)
	_, err = admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(payload)}})
	must(t, err)
	awaitMediaReady(t, admin, ref)
	waitForWithin(t, 45*time.Second, func() bool {
		_, err := admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
		var pending *ck.Error
		if errors.As(err, &pending) && pending.Status == 409 && pending.Code == "not_ready" {
			return false
		}
		must(t, err)
		return true
	})
	key, err := access.ContentKey(a.cfg.Scope.AppID.String(), ref.ID)
	must(t, err)
	product, err := b.merchant.CreateProduct(t.Context(), billing.CreateProductParams{Key: "paid-gallery", DisplayName: "Paid gallery", Entitlements: []string{key.String()}})
	must(t, err)
	t.Run("CK-J06-S04_CK-J13-S07_revocation_and_expiry_remove_fresh_media_and_archive_access", func(t *testing.T) {
		for _, mode := range []string{"revoke", "expire"} {
			t.Run(mode, func(t *testing.T) {
				user, credential := w.person(t, 0)
				customer, err := billing.ParseCustomerID(user.ID)
				must(t, err)
				_, err = b.merchant.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
				must(t, err)
				hours := 1
				grants, err := b.merchant.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID, Hours: &hours}}})
				must(t, err)
				if mode == "expire" {
					// Issue a still-valid download ticket immediately before the
					// entitlement ends, so ticket expiry cannot mask a stale grant.
					before := grants[0].EndsAt.Add(-time.Second)
					b.clock.Advance(before.Sub(b.clock.Now()))
					w.clock.set(before)
				}
				client := a.client(credential)
				var download versions.FileDownload
				waitFor(t, func() bool {
					reply, err := client.GetContentVersionDownload(t.Context(), ref.ID, ref.Version)
					must(t, err)
					if reply.Status == 202 {
						return false
					}
					must(t, reply.Decode(&download))
					return download.URL != ""
				})
				read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
				must(t, err)
				if len(read.Files) != 1 || read.Files[0].URL == "" {
					t.Fatalf("paid reader did not receive actual rendition: %+v", read)
				}
				base, err := url.Parse(fmt.Sprintf("%s/content/v1/contents/%s/versions/%s/download", a.server.URL, ref.ID, ref.Version))
				must(t, err)
				relative, err := url.Parse(download.URL)
				must(t, err)
				archiveURL := base.ResolveReference(relative).String()
				response, err := http.Get(archiveURL)
				must(t, err)
				data, err := io.ReadAll(response.Body)
				response.Body.Close()
				must(t, err)
				if response.StatusCode != 200 {
					t.Fatalf("authorized ZIP status=%d body=%s", response.StatusCode, data)
				}
				archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
				must(t, err)
				if len(archive.File) != 1 {
					t.Fatalf("ZIP contains %d files; expected the one authorized page", len(archive.File))
				}
				if mode == "revoke" {
					must(t, b.merchant.RevokeProductAccess(t.Context(), grants[0].ID, billing.RevokeProductAccessParams{Reason: "Revoke gallery"}))
				} else {
					b.clock.Advance(grants[0].EndsAt.Sub(b.clock.Now()))
					w.clock.set(*grants[0].EndsAt)
				}
				read, err = client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
				must(t, err)
				if len(read.Files) != 0 {
					t.Fatalf("ended entitlement still issued private media: %+v", read.Files)
				}
				_, err = client.GetContentVersionDownload(t.Context(), ref.ID, ref.Version)
				requireError(t, err, 403, "forbidden")
				response, err = http.Get(archiveURL)
				must(t, err)
				response.Body.Close()
				if response.StatusCode != 403 {
					t.Fatalf("prior ZIP grant bypassed current access: %d", response.StatusCode)
				}
			})
		}
	})
}
