package critical_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
)

// TestInternalArea follows a versioned upload: its derivatives are prepared in
// internal/, editors read them through exact storage capabilities, reader
// tokens never open them through the gateway, and release copies them to the
// served areas.
func TestInternalArea(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	m := attachMedia(t, w, a)
	admin := a.engine.Client()
	ctx := t.Context()
	ref := galleryRef(t, admin, "Internal preparation")
	_, err := admin.IngestMedia(ctx, http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{
		{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))},
		{Path: "cover.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))},
	})
	must(t, err)
	awaitMediaReady(t, admin, ref)
	config, err := a.cfg.MediaKinds()
	must(t, err)
	registry, err := mediastore.NewRegistry(config, mediastore.Hooks{})
	must(t, err)
	journal, err := mediastore.NewPGJournal(w.pool, w.schema, nil)
	must(t, err)
	manifests, err := mediastore.NewManifests(m.store, registry, mediastore.ManifestOptions{Locker: mediastore.PGLocker(w.pool), Journal: journal})
	must(t, err)
	target, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
	must(t, err)
	item, err := registry.Item(target)
	must(t, err)
	fetch := func(t *testing.T, address string) (int, []byte) {
		t.Helper()
		response, err := http.Get(address)
		must(t, err)
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		must(t, err)
		return response.StatusCode, body
	}
	var page string // the reader output's internal/ key

	t.Run("CK-J09-S01_versioned_derivatives_are_prepared_in_internal", func(t *testing.T) {
		prepared, _, err := manifests.Get(ctx, target)
		must(t, err)
		outputs := 0
		for _, file := range prepared.Files {
			want := layout.AreaInternal
			if file.IsUpload() {
				want = layout.AreaOriginals
			}
			if area, _, valid := layout.SplitBlobRef(file.Blob); !valid || area != want {
				t.Fatalf("%s prepared %q outside %s/", file.Path, file.Blob, want)
			}
			if !file.IsUpload() {
				outputs++
			}
			for _, publication := range file.Public {
				for _, blob := range publication.Blobs {
					if area, _, valid := layout.SplitBlobRef(blob); !valid || area != layout.AreaInternal {
						t.Fatalf("%s rendition prepared %q outside internal/", file.Path, blob)
					}
					outputs++
				}
			}
			if file.Path == "reader/one.webp" {
				page, err = item.Blob(file.Blob)
				must(t, err)
			}
		}
		if outputs != 3 || !strings.HasPrefix(page, item.InternalPrefix()) {
			t.Fatalf("derivatives missing from internal/: %d outputs, page %q", outputs, page)
		}
	})

	t.Run("CK-J05-S04_editors_read_internal_and_readers_cannot", func(t *testing.T) {
		read, err := admin.GetMediaVersion(ctx, ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
		must(t, err)
		var capability *url.URL
		for _, file := range read.Files {
			if file.Path == "reader/one.webp" {
				capability, err = url.Parse(file.URL)
				must(t, err)
			}
		}
		if capability == nil || capability.Path != "/"+m.bucket+"/"+page || capability.Query().Get("X-Amz-Signature") == "" {
			t.Fatalf("editor read of a prepared output is not an exact internal/ capability: %v", capability)
		}
		status, body := fetch(t, capability.String())
		object, _, err := m.store.Get(ctx, page, media.GetOptions{})
		must(t, err)
		stored, err := io.ReadAll(object)
		object.Close()
		must(t, err)
		if status != http.StatusOK || !bytes.Equal(body, stored) {
			t.Fatalf("editor capability: HTTP %d", status)
		}
		ring, err := token.NewRing(m.key, nil)
		must(t, err)
		exp := time.Now().Add(time.Minute)
		for name, credential := range map[string]string{
			"item":         ring.Sign(strings.TrimSuffix(item.Prefix(), "/"), exp),
			"private file": ring.Sign(token.PrivateFileScope(page), exp),
			"none":         "",
		} {
			if status, _ := fetch(t, m.gateway.URL+layout.URLPrefix+page+"?t="+credential); status != http.StatusNotFound {
				t.Fatalf("%s reader token opened internal/ through the gateway: HTTP %d", name, status)
			}
		}
		if unreleased, err := a.client("").GetMediaVersion(ctx, ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{}); err == nil && len(unreleased.Files) != 0 {
			t.Fatalf("reader listed unreleased internal/ files: %+v", unreleased.Files)
		}
	})

	t.Run("CK-J09-S01_release_copies_internal_outputs_to_served_areas", func(t *testing.T) {
		_, err := admin.PostContentPublish(ctx, ref.ID, catalog.PublishInput{})
		must(t, err)
		awaitEditionPublication(t, a, admin, ref.ID, ref.Version)
		read, err := a.client("").GetMediaVersion(ctx, ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
		must(t, err)
		// A free edition is released to public/, a restricted one to private/.
		served := func(address string) bool {
			return strings.Contains(address, item.PublicPrefix()) || strings.Contains(address, item.PrivatePrefix())
		}
		if len(read.Files) != 1 || read.Files[0].Path != "reader/one.webp" || !served(read.Files[0].URL) || len(read.Public) != 1 || len(read.Public[0].Renditions) != 2 {
			t.Fatalf("release did not copy internal/ outputs to served areas: %+v", read)
		}
		if status, _ := fetch(t, read.Files[0].URL); status != http.StatusOK {
			t.Fatalf("released page: HTTP %d", status)
		}
		for _, rendition := range read.Public[0].Renditions {
			if !strings.Contains(rendition.URL, item.PublicPrefix()) {
				t.Fatalf("cover not public: %s", rendition.URL)
			}
			assertImage(t, rendition.URL, rendition.W, rendition.H)
		}
		if _, err := m.store.Head(ctx, page); !errors.Is(err, mediastore.ErrNotFound) {
			t.Fatalf("release kept the copied internal/ source: %v", err)
		}
	})
}
