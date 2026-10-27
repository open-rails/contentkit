package critical_test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
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
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/openrails/billing"
)

func TestCommerceMediaGrantRevocation(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	b := attachBilling(t, w, a)
	storage := attachMedia(t, w, a, func(c *ck.Config) {
		kind := &c.Media.Config.Kinds[0]
		kind.Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}, Download: "archive"}
		kind.Private = append(kind.Private,
			media.Private{Name: "archive-pages", From: "pages/{name}", To: "download-pages/{name}{ext}", Image: &media.Image{Format: media.ImageFormatSource}},
			media.Private{Name: "archive", To: "pages.zip", Zip: "download-pages/", HostOnly: true})
	})
	admin := a.engine.Client()
	ref := galleryRef(t, admin, "Paid gallery")
	_, err := admin.PatchContentVersion(t.Context(), ref.ID, ref.Version, catalog.VersionInput{Access: &access.Rule{Level: access.PPV}})
	must(t, err)
	payload := imageBytes(t)
	_, err = admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(payload)}})
	must(t, err)
	awaitMediaReady(t, admin, ref)
	_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, a, admin, ref.ID, ref.Version)
	key, err := access.VersionKey(a.cfg.Scope.AppID.String(), ref.ID, ref.Version)
	must(t, err)
	product, err := b.merchant.CreateProduct(t.Context(), billing.CreateProductParams{Key: "paid-gallery", DisplayName: "Paid gallery", Entitlements: []string{key.String()}})
	must(t, err)
	assertDownloadSourceUnserved := func(readerURL string) {
		config, err := a.cfg.MediaKinds()
		must(t, err)
		registry, err := media.NewRegistry(config)
		must(t, err)
		target, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
		must(t, err)
		item, err := registry.Item(target)
		must(t, err)
		journal, err := media.NewPGJournal(w.pool, w.schema, nil)
		must(t, err)
		manifests, err := media.NewManifests(storage.store, registry, media.ManifestOptions{Locker: media.PGLocker(w.pool), Journal: journal})
		must(t, err)
		manifest, _, err := manifests.Get(t.Context(), target)
		must(t, err)
		highResolution, ok := manifest.Get("download-pages/one.png")
		if !ok {
			t.Fatal("download-only source rendition is missing")
		}
		if area, _, valid := layout.SplitBlobRef(highResolution.Blob); !valid || area != layout.AreaInternal {
			t.Fatalf("download-only source was promoted into the item token's served area: %+v", highResolution)
		}
		key, err := item.Blob(highResolution.Blob)
		must(t, err)
		issued, err := url.Parse(readerURL)
		must(t, err)
		response, err := http.Get(storage.gateway.URL + layout.URLPrefix + key + "?" + issued.RawQuery)
		must(t, err)
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("item token bypassed download policy through high-resolution origin: HTTP %d", response.StatusCode)
		}
	}
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
				assertDownloadSourceUnserved(read.Files[0].URL)
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
	t.Run("CK-J05-S04_CK-J13-S07_segment_and_audience_deadlines_bound_reads_and_archives", func(t *testing.T) {
		ref := galleryRef(t, admin, "Timed chapters")
		base := b.clock.Now()
		chapterAt, publicAt := base.Add(time.Hour), base.Add(2*time.Hour)
		_, err := admin.PatchContentVersion(t.Context(), ref.ID, ref.Version, catalog.VersionInput{Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}, Releases: []access.Release{{At: publicAt, Level: access.Public}}}})
		must(t, err)
		secondImage := image.NewNRGBA(image.Rect(0, 0, 80, 60))
		for y := 0; y < 60; y++ {
			for x := 0; x < 80; x++ {
				secondImage.SetNRGBA(x, y, color.NRGBA{R: uint8(255 - x*2), G: uint8(y * 3), B: 200, A: 255})
			}
		}
		var secondPayload bytes.Buffer
		must(t, png.Encode(&secondPayload, secondImage))
		_, err = admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{
			{Path: "pages/first.png", Type: "image/png", Body: bytes.NewReader(payload)},
			{Path: "pages/second.png", Type: "image/png", Body: bytes.NewReader(secondPayload.Bytes())},
		})
		must(t, err)
		awaitMediaReady(t, admin, ref)
		_, err = admin.PutContentVersionSegments(t.Context(), ref.ID, ref.Version, catalog.SegmentsInput{Segments: []catalog.SegmentInput{{Position: 0, Title: "Available chapter", StartPage: "pages/first.png"}, {Position: 1, Title: "Later chapter", StartPage: "pages/second.png", ReleaseAt: &chapterAt}}})
		must(t, err)
		_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
		must(t, err)
		awaitEditionPublication(t, a, admin, ref.ID, ref.Version)
		user, credential := w.person(t, 0)
		customer, err := billing.ParseCustomerID(user.ID)
		must(t, err)
		_, err = b.merchant.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
		must(t, err)
		gold, err := b.merchant.CreateProduct(t.Context(), billing.CreateProductParams{Key: "scheduled-gold", DisplayName: "Early tier", Entitlements: []string{"gold"}})
		must(t, err)
		_, err = b.merchant.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: gold.ID}}})
		must(t, err)
		reader, anonymous := a.client(credential), a.client("")
		setTime := func(at time.Time) { b.clock.Advance(at.Sub(b.clock.Now())); w.clock.set(at) }
		assertFiles := func(client *ck.Client, count int) {
			read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
			must(t, err)
			if len(read.Files) != count || read.Total != count {
				t.Fatalf("timed reader got %d files/total%d, expected%d", len(read.Files), read.Total, count)
			}
		}
		awaitFiles := func(client *ck.Client, count int) {
			deadline := time.Now().Add(45 * time.Second)
			for time.Now().Before(deadline) {
				wakeDuePublicationJobs(t, a)
				read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
				if err == nil && len(read.Files) == count && read.Total == count {
					return
				}
				time.Sleep(550 * time.Millisecond)
			}
			assertFiles(client, count)
		}
		archive := func(client *ck.Client, want int) ck.GetContentVersionArchiveQuery {
			var grant versions.FileDownload
			waitFor(t, func() bool {
				response, err := client.GetContentVersionDownload(t.Context(), ref.ID, ref.Version)
				must(t, err)
				if response.Status == 202 {
					return false
				}
				must(t, response.Decode(&grant))
				return grant.URL != ""
			})
			u, err := url.Parse(grant.URL)
			must(t, err)
			q := u.Query()
			query := ck.GetContentVersionArchiveQuery{For: q.Get("for"), Account: q.Get("account"), AccountSet: q.Get("account_set"), Kind: q.Get("kind"), Editor: q.Get("editor"), Claims: q.Get("claims"), Mark: q.Get("mark"), Name: q.Get("name"), Revision: q.Get("revision"), Pages: q.Get("pages"), Ticket: q.Get("ticket")}
			response, err := client.GetContentVersionArchive(t.Context(), ref.ID, ref.Version, query)
			must(t, err)
			zipFile, err := zip.NewReader(bytes.NewReader(response.Body), int64(len(response.Body)))
			must(t, err)
			if len(zipFile.File) != want {
				t.Fatalf("timed ZIP contains%d pages, expected%d", len(zipFile.File), want)
			}
			return query
		}
		awaitFiles(reader, 1)
		awaitPublicationJobsIdle(t, w, a)
		setTime(chapterAt.Add(-time.Microsecond))
		assertFiles(reader, 1)
		assertFiles(anonymous, 0)
		firstTicket := archive(reader, 1)
		firstTicket.Pages = "2"
		_, err = reader.GetContentVersionArchive(t.Context(), ref.ID, ref.Version, firstTicket)
		requireError(t, err, 404, "not_found")
		setTime(chapterAt)
		assertFiles(reader, 1) // time alone does not promote prepared files
		wakeDuePublicationJobs(t, a)
		awaitFiles(reader, 2)
		assertFiles(anonymous, 0)
		archive(reader, 2)
		awaitPublicationJobsIdle(t, w, a)
		setTime(publicAt.Add(-time.Microsecond))
		assertFiles(anonymous, 0)
		setTime(publicAt)
		assertFiles(anonymous, 0) // public policy activates with completed placement
		wakeDuePublicationJobs(t, a)
		awaitFiles(anonymous, 2)
		archive(anonymous, 2)
		// Opening an audience does not grant billing rights to an unpurchased
		// account: completed public publication does not create a purchase.
		unpaid, _ := w.person(t, 0)
		unpaidID, err := billing.ParseCustomerID(unpaid.ID)
		must(t, err)
		rights, err := b.merchant.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: unpaidID, Entitlements: []string{"gold"}})
		must(t, err)
		if rights.Entitlements["gold"] {
			t.Fatal("public audience release granted a billing tier")
		}
	})
}
