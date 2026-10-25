package critical_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
)

// This resolver is the trusted authorization seam of the standalone media
// module. Catalog/authentication behavior is exercised by the full host tests;
// this case exercises actual manifests, S3 copies, signatures and the gateway.
type completedReaderAuthority struct{ editor string }

func (a *completedReaderAuthority) Resolve(_ context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]resolve.Resolution, error) {
	out := map[contentref.ContentKey]resolve.Resolution{}
	for _, ref := range refs {
		out[ref.Key()] = resolve.Resolution{Visible: true, Accessible: actor.ID != "", Editor: actor.ID == a.editor}
	}
	return out, nil
}

type noEditorReadStore struct{ media.Store }

func TestCompletedPublicationReader(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	storage := newMediaStorage(t, *a.cfg.Scope)
	editor := access.Actor{ID: uuid.NewString()}
	viewer := access.Actor{ID: uuid.NewString()}
	authority := &completedReaderAuthority{editor: editor.ID}
	var active string
	omitIndex, leakPreparation := false, false
	var trackSource, indexSource, publicSource string
	cfg := media.Config{Scope: layout.Scope{TenantID: a.cfg.Scope.TenantID.String(), AppID: a.cfg.Scope.AppID.String()}, BaseURL: storage.gateway.URL,
		Kinds: []media.Kind{{Name: "clip", Versions: true, KeepOriginals: true,
			Uploads: []media.Upload{{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 1 << 20}, {Path: "archive", Types: []string{"application/zip"}, MaxBytes: 1 << 20}},
			Private: []media.Private{{Name: "stream", From: "source", To: "streams/", HLS: &media.HLS{Ladder: []int{144}}}},
		}},
		Hooks: media.Hooks{Resolver: authority},
	}
	cfg.Hooks.ReaderManifest = func(_ context.Context, _ contentref.ContentRef, _ access.Actor, current *media.Manifest) (*media.Manifest, error) {
		out := current.Clone()
		out.Deliveries = nil
		if leakPreparation {
			return out, nil // a malformed projection must still not mint future URLs
		}
		out.Files = nil
		for _, delivery := range current.Deliveries {
			if delivery.ID != active || !delivery.Ready || delivery.Withdrawn {
				continue
			}
			mapping := map[string]string{}
			for _, f := range delivery.Files {
				mapping[f.Source] = f.Blob
			}
			if omitIndex {
				var kept []media.DeliveryFile
				for _, f := range delivery.Files {
					if f.Source != indexSource {
						kept = append(kept, f)
					}
				}
				delivery.Files = kept
			}
			out.Deliveries = append(out.Deliveries, delivery)
			for _, f := range current.Files {
				if blob := mapping[f.Blob]; blob != "" {
					f.Blob = blob
					out.Files = append(out.Files, f)
				}
			}
		}
		return out, nil
	}
	registry, err := media.NewRegistry(cfg)
	must(t, err)
	parent, err := uuid.NewV7()
	must(t, err)
	version, err := uuid.NewV7()
	must(t, err)
	ref, err := registry.VersionRef("clip", parent.String(), version.String())
	must(t, err)
	item, err := registry.Item(ref)
	must(t, err)
	journal, err := media.NewPGJournal(w.pool, w.schema, nil)
	must(t, err)
	manifests, err := media.NewManifests(storage.store, registry, media.ManifestOptions{Locker: media.PGLocker(w.pool), Journal: journal})
	must(t, err)
	put := func(area, kind string, body []byte) string {
		sum := sha256.Sum256(body)
		blobs, err := manifests.NewBlobs(t.Context(), ref, area, [][]byte{sum[:]})
		must(t, err)
		key, err := item.Blob(blobs[0])
		must(t, err)
		_, err = storage.store.Put(t.Context(), key, bytes.NewReader(body), int64(len(body)), media.PutOptions{ContentType: kind, ChecksumSHA256: sum[:], IfNoneMatch: "*"})
		must(t, err)
		return blobs[0]
	}
	original := put(layout.AreaOriginals, "video/mp4", []byte("retained original source"))
	archive := put(layout.AreaOriginals, "application/zip", []byte("retained original archive"))
	trackBytes := []byte("fixture-init-and-one-byte-range-segment")
	trackSource = put(layout.AreaFuture, "video/mp4", trackBytes)
	index, err := json.Marshal(media.TrackIndex{Segments: []media.Segment{{Offset: 8, Length: int64(len(trackBytes) - 8), Seconds: 1}}})
	must(t, err)
	indexSource = put(layout.AreaFuture, "application/json", index)
	publicSource = put(layout.AreaFuture, "video/mp4", []byte("released public preview"))
	_, err = manifests.EditExisting(t.Context(), ref, func(m *media.Manifest) error {
		m.Files = []media.File{
			{Path: "source.mp4", Blob: original, Type: "video/mp4", Size: 24},
			{Path: "archive.zip", Blob: archive, Type: "application/zip", Size: 25},
			{Path: "streams/h264.mp4", Blob: trackSource, Type: "video/mp4", Size: int64(len(trackBytes)), From: "source.mp4", Preset: "stream", FP: "fixture", Track: &media.Track{Kind: media.TrackVideo, Codec: "h264", Codecs: "avc1", Index: indexSource}},
			{Path: "streams/preview.mp4", Blob: publicSource, Type: "video/mp4", Size: 23, From: "source.mp4", Preset: "stream", FP: "fixture"},
		}
		return nil
	})
	must(t, err)
	options := media.ReaderOptions{Manifests: manifests, Delivery: media.Delivery{Mode: media.DeliverURL, SigningKey: storage.key, TTL: time.Hour}}
	reader, err := media.NewReader(options)
	must(t, err)
	read := func(actor access.Actor, editor bool) *media.ReadResult {
		out, err := reader.Read(t.Context(), ref, actor, media.ReadOptions{Editor: editor})
		must(t, err)
		return out
	}
	if out := read(viewer, false); len(out.Files) != 0 {
		t.Fatalf("prepared data appeared before publication: %+v", out)
	}
	if out := read(editor, false); len(out.Files) != 0 {
		t.Fatal("editor authority bypassed the reader projection without explicit editor mode")
	}
	get := func(address string, want int) *http.Response {
		t.Helper()
		response, err := http.Get(address)
		must(t, err)
		if response.StatusCode != want {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			t.Fatalf("HTTP %d want %d: %s", response.StatusCode, want, body)
		}
		return response
	}
	edit := read(editor, true)
	if len(edit.Files) != 4 || edit.Expires > time.Now().Add(5*time.Minute).Unix() {
		t.Fatalf("editor scope or expiry is wrong: %+v", edit)
	}
	for _, file := range edit.Files {
		if file.URL == "" || strings.HasPrefix(file.URL, storage.gateway.URL) {
			t.Fatalf("unserved editor file has no exact storage capability: %+v", file)
		}
		response := get(file.URL, http.StatusOK)
		response.Body.Close()
		if response.Header.Get("Cache-Control") != "private, no-store" {
			t.Fatal("editor capability omitted signed no-store policy")
		}
	}
	leakPreparation = true
	if out := read(viewer, false); len(out.Files) != 0 {
		t.Fatal("unserved areas were signed from an invalid reader projection")
	}
	leakPreparation = false
	operation := uuid.NewString()
	_, err = manifests.PreparePromotion(t.Context(), ref, operation, []media.Promotion{{Source: trackSource, Area: layout.AreaPrivate}, {Source: indexSource, Area: layout.AreaPrivate}, {Source: publicSource, Area: layout.AreaPublic}})
	must(t, err)
	_, err = manifests.Promote(t.Context(), ref, operation)
	must(t, err)
	if out := read(viewer, false); len(out.Files) != 0 {
		t.Fatal("physical completion replaced catalog publication authority")
	}
	active = operation
	out := read(viewer, false)
	if out.Access != media.AccessFull || len(out.Files) != 2 {
		t.Fatalf("released item was not a whole-folder grant: %+v", out)
	}
	ring, err := token.NewRing(storage.key, nil)
	must(t, err)
	for _, file := range out.Files {
		address, err := url.Parse(file.URL)
		must(t, err)
		if strings.Contains(address.Path, "/public/") && address.Query().Get("t") != "" {
			t.Fatal("public delivery carried a private token")
		}
		if strings.Contains(address.Path, "/private/") {
			must(t, ring.VerifyScope(address.Query().Get("t"), strings.TrimSuffix(item.Prefix(), "/"), time.Now()))
			for _, blob := range []string{original, archive, trackSource} {
				key, err := item.Blob(blob)
				must(t, err)
				response := get(storage.gateway.URL+layout.URLPrefix+key+"?t="+address.Query().Get("t"), http.StatusNotFound)
				response.Body.Close()
			}
		}
		response := get(file.URL, http.StatusOK)
		response.Body.Close()
	}
	for _, blob := range []string{original, archive, trackSource} {
		_, _, err := reader.DownloadURL(item, media.File{Path: "forbidden", Blob: blob}, "file.mp4", time.Minute)
		if !errors.Is(err, media.ErrNotAllowed) {
			t.Fatalf("download exposed an unserved file: %s: %v", blob, err)
		}
	}
	authority.editor = ""
	for _, file := range read(editor, true).Files {
		if !strings.HasPrefix(file.URL, storage.gateway.URL) {
			t.Fatal("revoked editor authority still minted an original storage capability")
		}
	}
	authority.editor = editor.ID
	grant, err := reader.Grant(t.Context(), ref, viewer)
	must(t, err)
	body, err := grant.MediaPlaylist(t.Context(), "streams/h264.mp4")
	must(t, err)
	if strings.Contains(string(body), "/future/") || !strings.Contains(string(body), "/private/") {
		t.Fatalf("HLS did not use the completed delivery: %s", body)
	}
	omitIndex = true
	grant, err = reader.Grant(t.Context(), ref, viewer)
	must(t, err)
	_, err = grant.MediaPlaylist(t.Context(), "streams/h264.mp4")
	if !errors.Is(err, media.ErrNotAllowed) {
		t.Fatalf("unmapped index was read through application storage: %v", err)
	}
	omitIndex = false
	withoutEditor, err := media.NewManifests(noEditorReadStore{storage.store}, registry, media.ManifestOptions{Locker: media.PGLocker(w.pool), Journal: journal})
	must(t, err)
	options.Manifests = withoutEditor
	unsupported, err := media.NewReader(options)
	must(t, err)
	_, err = unsupported.Read(t.Context(), ref, editor, media.ReadOptions{Editor: true})
	if !errors.Is(err, media.ErrEditorReadUnsupported) {
		t.Fatalf("custom store returned a broken editor gateway URL: %v", err)
	}
}
