package critical_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/openrails/billing"
)

// TestLockedPreviewPages follows a paid version's free pages: release copies
// exactly those leading released pages' renditions to public/, every
// non-payer's reader plan lists them as unsigned URLs, and a rule edit
// retires copies the new preview no longer names.
func TestLockedPreviewPages(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	b := attachBilling(t, w, a)
	m := attachMedia(t, w, a, func(c *ck.Config) {
		k := &c.Media.Kinds[0]
		k.Uploads[0].Max = 20
		k.Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	admin, anonymous := a.engine.Client(), a.client("")
	config, err := a.cfg.MediaKinds()
	must(t, err)
	registry, err := mediastore.NewRegistry(config, mediastore.Hooks{})
	must(t, err)
	journal, err := mediastore.NewPGJournal(w.pool, w.schema, nil)
	must(t, err)
	manifests, err := mediastore.NewManifests(m.store, registry, mediastore.ManifestOptions{Locker: mediastore.PGLocker(w.pool), Journal: journal})
	must(t, err)
	paid := func(pages int) *access.Rule {
		rule := &access.Rule{Level: access.PPV}
		if pages > 0 {
			rule.Preview = &access.Preview{Pages: pages}
		}
		return rule
	}
	gallery := func(title string, rule *access.Rule, pages int) (media.RefBody, mediastore.Item) {
		t.Helper()
		ref := galleryRef(t, admin, title)
		_, err := admin.PatchContentVersion(t.Context(), ref.ID, ref.Version, catalog.VersionInput{Access: rule})
		must(t, err)
		var files []ck.MediaFile
		for i := range pages {
			files = append(files, ck.MediaFile{Path: fmt.Sprintf("pages/%02d.png", i+1), Type: "image/png", Body: bytes.NewReader(chapterImage(t, uint8(31+i*17)))})
		}
		_, err = admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), files)
		must(t, err)
		awaitMediaReady(t, admin, ref)
		target, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
		must(t, err)
		item, err := registry.Item(target)
		must(t, err)
		return ref, item
	}
	plan := func(client *ck.Client, ref media.RefBody) versions.ReaderPlan {
		t.Helper()
		reply, err := client.GetContentVersionReader(t.Context(), ref.ID, ref.Version)
		must(t, err)
		if reply.Status != http.StatusOK {
			t.Fatalf("reader plan returned %d: %s", reply.Status, reply.Body)
		}
		var out versions.ReaderPlan
		must(t, reply.Decode(&out))
		return out
	}
	indexes := func(p versions.ReaderPlan) []int {
		out := []int{}
		for _, preview := range p.Previews {
			out = append(out, preview.Index)
		}
		return out
	}
	status := func(address string) int {
		t.Helper()
		response, err := http.Get(address)
		must(t, err)
		response.Body.Close()
		return response.StatusCode
	}
	// placement maps each reader rendition to its area, and checks that
	// public/ holds exactly the manifest's public objects (no stray copy).
	placement := func(ref media.RefBody, item mediastore.Item) (map[string]string, bool) {
		t.Helper()
		target, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
		must(t, err)
		manifest, _, err := manifests.Get(t.Context(), target)
		must(t, err)
		areas, public := map[string]string{}, []string{}
		for _, f := range manifest.Files {
			blobs := []string{f.Blob}
			for _, pub := range f.Public {
				blobs = append(blobs, pub.Blobs...)
			}
			for _, blob := range blobs {
				if area, name, valid := layout.SplitBlobRef(blob); valid && area == layout.AreaPublic {
					public = append(public, name)
				}
			}
			if strings.HasPrefix(f.Path, "reader/") {
				area, _, _ := layout.SplitBlobRef(f.Blob)
				areas[f.Path] = area
			}
		}
		stored := []string{}
		for object, err := range m.store.List(t.Context(), item.PublicPrefix()) {
			must(t, err)
			if name := strings.TrimPrefix(object.Key, item.PublicPrefix()); name != layout.FenceName {
				stored = append(stored, name)
			}
		}
		slices.Sort(public)
		slices.Sort(stored)
		return areas, slices.Equal(public, stored)
	}
	awaitPreviews := func(ref media.RefBody, item mediastore.Item, want []int, public ...string) versions.ReaderPlan {
		t.Helper()
		var got versions.ReaderPlan
		var areas map[string]string
		exact := false
		waitForChapterPublication(t, func() bool {
			wakeDuePublicationJobs(t, a)
			got = plan(anonymous, ref)
			areas, exact = placement(ref, item)
			for path, area := range areas {
				if slices.Contains(public, path) != (area == layout.AreaPublic) {
					return false
				}
			}
			return exact && slices.Equal(indexes(got), want)
		})
		return got
	}
	unsigned := func(p versions.ReaderPlan, item mediastore.Item) {
		t.Helper()
		for _, preview := range p.Previews {
			address, err := url.Parse(preview.URL)
			must(t, err)
			if address.RawQuery != "" || !strings.Contains(address.Path, item.PublicPrefix()) || preview.ID == "" || preview.W <= 0 {
				t.Fatalf("preview %d is not a plain public URL: %+v", preview.Index, preview)
			}
			if got := status(preview.URL); got != http.StatusOK {
				t.Fatalf("preview %d: HTTP %d", preview.Index, got)
			}
		}
	}

	ref, item := gallery("Paid gallery with free pages", paid(2), 5)
	_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, a, admin, ref.ID, ref.Version)
	var first versions.ReaderPlan

	t.Run("CK-J06-S08_non_payers_read_exactly_the_free_pages_publicly", func(t *testing.T) {
		first = awaitPreviews(ref, item, []int{0, 1}, "reader/01.webp", "reader/02.webp")
		unsigned(first, item)
		for _, segment := range first.Segments {
			if !segment.Locked || segment.Item != nil || len(segment.Pages) != 0 {
				t.Fatalf("non-payer plan exposes locked pages: %+v", segment)
			}
		}
		target, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
		must(t, err)
		manifest, _, err := manifests.Get(t.Context(), target)
		must(t, err)
		third, ok := manifest.Get("reader/03.webp")
		if area, _, _ := layout.SplitBlobRef(third.Blob); !ok || area != layout.AreaPrivate {
			t.Fatalf("a page past the preview left private/: %+v", third)
		}
		key, err := item.Blob(third.Blob)
		must(t, err)
		ring, err := token.NewRing(m.key, nil)
		must(t, err)
		signed := m.gateway.URL + layout.URLPrefix + key + "?t=" + url.QueryEscape(ring.Sign(token.PrivateFileScope(key), time.Now().Add(time.Minute)))
		if got := status(signed); got != http.StatusOK {
			t.Fatalf("a page past the preview is not served with its exact-file token: %d", got)
		}
		// The gateway refuses as not found, so a refusal does not confirm the key.
		if got := status(m.gateway.URL + layout.URLPrefix + key); got != http.StatusNotFound {
			t.Fatalf("a page past the preview answers %d without a token, want the gateway's refusal", got)
		}
		read, err := anonymous.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
		must(t, err)
		listed := []string{}
		for _, f := range read.Files {
			if !strings.Contains(f.URL, item.PublicPrefix()) {
				t.Fatalf("non-payer read lists a private file: %+v", f)
			}
			listed = append(listed, f.Path)
		}
		if !slices.Equal(listed, []string{"reader/01.webp", "reader/02.webp"}) {
			t.Fatalf("non-payer read lists %v, want only the free pages", listed)
		}
	})

	t.Run("CK-J06-S08_entitled_reader_reads_every_page_and_no_previews", func(t *testing.T) {
		key, err := access.VersionKey(a.cfg.Scope.AppID.String(), ref.ID, ref.Version)
		must(t, err)
		product, err := b.merchant.CreateProduct(t.Context(), billing.CreateProductParams{Key: "preview-gallery", DisplayName: "Preview gallery", Entitlements: []string{key.String()}})
		must(t, err)
		user, credential := w.person(t, 0)
		customer, err := billing.ParseCustomerID(user.ID)
		must(t, err)
		_, err = b.merchant.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
		must(t, err)
		_, err = b.merchant.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID}}})
		must(t, err)
		got := plan(a.client(credential), ref)
		pages := 0
		for _, segment := range got.Segments {
			if segment.Locked {
				t.Fatalf("entitled reader has a locked segment: %+v", segment)
			}
			pages += len(segment.Pages)
		}
		if pages != 5 || len(got.Previews) != 0 {
			t.Fatalf("entitled plan: %d pages, previews %+v", pages, got.Previews)
		}
	})

	t.Run("CK-J06-S08_shrinking_the_preview_retires_its_public_copies", func(t *testing.T) {
		_, err := admin.PatchContentVersion(t.Context(), ref.ID, ref.Version, catalog.VersionInput{Access: paid(1)})
		must(t, err)
		unsigned(awaitPreviews(ref, item, []int{0}, "reader/01.webp"), item)
		if got := status(first.Previews[1].URL); got != http.StatusNotFound {
			t.Fatalf("retired second preview: HTTP %d", got)
		}
		_, err = admin.PatchContentVersion(t.Context(), ref.ID, ref.Version, catalog.VersionInput{Access: paid(0)})
		must(t, err)
		awaitPreviews(ref, item, []int{})
		if got := status(first.Previews[0].URL); got != http.StatusNotFound {
			t.Fatalf("a rule without a preview still serves its first page: HTTP %d", got)
		}
	})

	t.Run("CK-J06-S08_scheduled_chapter_never_contributes_previews", func(t *testing.T) {
		ref, item := gallery("Paid chapters with free pages", paid(3), 4)
		future := w.clock.now().Add(time.Hour)
		_, err := admin.PutContentVersionSegments(t.Context(), ref.ID, ref.Version, catalog.SegmentsInput{Segments: []catalog.SegmentInput{
			{Position: 0, StartPage: "pages/01.png"},
			{Position: 1, StartPage: "pages/03.png", ReleaseAt: &future},
		}})
		must(t, err)
		_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
		must(t, err)
		awaitEditionPublication(t, a, admin, ref.ID, ref.Version)
		unsigned(awaitPreviews(ref, item, []int{0, 1}, "reader/01.webp", "reader/02.webp"), item)
	})
}
