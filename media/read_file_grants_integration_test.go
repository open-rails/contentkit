package media_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/media"
)

func TestReadRestrictedFilesNeverGrantsFolderOrOriginal(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.gallery(1, 2)
	kind, _ := f.reg.Kind("gallery")
	kind.ServeOriginals = true
	man, _, err := f.ms.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := man.Get("thumb/000.webp")
	last, _ := man.Get("high/001.webp")
	// This fixture's fake processor normally aliases source/output blobs.
	// Give the two selected renditions distinct immutable allocations.
	first.Blob = f.blob(ref, png(501), "image/png")
	last.Blob = f.blob(ref, png(502), "image/png")
	if _, err := f.ms.EditExisting(t.Context(), ref, func(m *media.Manifest) error {
		m.Files[m.Find(first.Path)].Blob = first.Blob
		m.Files[m.Find(last.Path)].Blob = last.Blob
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	original, _ := man.Get("originals/000.png")
	archive, _ := man.Get("download/pages.zip")
	f.res.set(cid(1), resolve.Resolution{Visible: true, Accessible: true, MediaFiles: map[string]bool{
		first.Blob: true, last.Blob: true,
	}})
	viewer := access.Actor{ID: "viewer", Kind: "user"}
	// Even a misconfigured allowlist cannot turn source uploads or archives
	// into a reader grant. Cookie mode must still produce exact-file URLs.
	rd, err := media.NewReader(media.ReaderOptions{Manifests: f.ms, DefaultLimit: 1,
		Delivery: media.Delivery{Mode: media.DeliverCookie, CookieDomain: "doujins.test", SigningKey: signKey}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := rd.Read(t.Context(), ref, viewer, media.ReadOptions{Paths: []string{last.Path}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Access != media.AccessPartial || got.Cookie != nil || got.Total != 2 || len(got.Files) != 2 || !got.Locked {
		t.Fatalf("restricted read: %+v", got)
	}
	if got.Files[0].URL != "" || got.Files[1].Path != last.Path || got.Files[1].URL == "" {
		t.Fatalf("exact path beyond default ordinal window was not signed: %+v", got.Files)
	}
	if status, _, _ := f.fetch(got.Files[1].URL); status != http.StatusOK {
		t.Fatalf("authorized file: %d", status)
	}
	signed, _ := url.Parse(got.Files[1].URL)
	item, _ := f.reg.Item(ref)
	for _, file := range []media.File{first, original, archive} {
		key, _ := item.Blob(file.Blob)
		other := *signed
		other.Path = "/v1/" + key
		if status, _, _ := f.fetch(other.String()); status != http.StatusNotFound {
			t.Fatalf("one file's token authorized %s: %d", file.Path, status)
		}
	}
	grant, err := rd.Grant(t.Context(), ref, viewer)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Full() || grant.Cookie() != nil {
		t.Fatal("restricted reader received whole-item authority")
	}
	// Even an accidentally broad allowlist must not expose upload or archive
	// entries, including when a legacy rendition aliases their same blob.
	f.res.set(cid(1), resolve.Resolution{Visible: true, Accessible: true, MediaFiles: map[string]bool{
		original.Blob: true, archive.Blob: true,
	}})
	grant, err = rd.Grant(t.Context(), ref, viewer)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []media.File{original, archive} {
		if _, err := grant.URL(file, false); !errors.Is(err, media.ErrNotAllowed) {
			t.Fatalf("signed %s: %v", file.Path, err)
		}
	}
	f.res.set(cid(1), resolve.Resolution{Visible: true, Accessible: true, MediaFiles: map[string]bool{}})
	empty, err := rd.Read(t.Context(), ref, viewer, media.ReadOptions{})
	if err != nil || empty.Access != media.AccessNone || len(empty.Files) != 0 || empty.Cookie != nil {
		t.Fatalf("empty exact grant widened to full: %+v %v", empty, err)
	}
}

func TestReadExactPathsHTTPAndBounds(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.gallery(1, 2)
	srv := httptest.NewServer(f.rd.Handler(media.HandlerOptions{Auth: identity{f.editor}, Limit: media.RateLimit{Disabled: true}}))
	defer srv.Close()
	q := url.Values{"path": {"high/001.webp", "thumb/000.webp"}}
	resp, err := srv.Client().Get(srv.URL + "/gallery/" + ref.ContentID + "?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got media.ReadResult
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	signed := map[string]bool{}
	for _, file := range got.Files {
		if file.URL != "" {
			signed[file.Path] = true
		}
	}
	if len(signed) != 2 || !signed["high/001.webp"] || !signed["thumb/000.webp"] || got.Total != 5 {
		t.Fatalf("paths changed metadata or signed outside selection: %+v", got)
	}
	for _, opts := range []media.ReadOptions{{Paths: make([]string, 65)}, {Paths: []string{""}}, {Paths: []string{"high/001.webp"}, Limit: 1}, {Paths: []string{"high/001.webp"}, Offset: 1}} {
		if _, err := f.rd.Read(t.Context(), ref, f.editor, opts); !errors.Is(err, media.ErrInvalidRequest) {
			t.Fatalf("accepted invalid paths: %+v %v", opts, err)
		}
	}
	unknown, err := f.rd.Read(t.Context(), ref, f.editor, media.ReadOptions{Paths: []string{"high/missing.webp"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range unknown.Files {
		if file.URL != "" {
			t.Fatal("missing path fell back to signing default window")
		}
	}
	body, _ := json.Marshal(unknown)
	if strings.Contains(string(body), "?t=") {
		t.Fatal("missing path returned a token")
	}
}
