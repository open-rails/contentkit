package versions_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

// A fork fills a new version with a sibling's pages, outputs, audio and
// segments, and publishes it.
func TestFork(t *testing.T) {
	s := newSite(t, siteOptions{})
	c, from := gallery(t, s)
	src := s.plan(s.creator, from, func(p *versions.ReaderPlan) bool { return p.Playback != nil })
	to := s.version(c)
	path := "/v1/contents/" + c + "/versions/" + to.Version + "/fork"
	body := versions.ForkBody{From: from.Version, Playback: true, Segments: true}
	if resp, _ := s.call(s.stranger, "POST", path, body); resp.StatusCode != 403 {
		t.Fatalf("a stranger forked: %d", resp.StatusCode)
	}
	if resp, b := s.call(s.creator, "POST", path, body); resp.StatusCode != 202 {
		t.Fatalf("fork: %d %s", resp.StatusCode, b)
	}
	got := s.plan(s.creator, to, func(p *versions.ReaderPlan) bool { return p.Playback != nil })
	if *got.Pages != *src.Pages || len(got.Segments) != len(src.Segments) || got.Segments[2].Title != "Part 3" ||
		got.Segments[1].Pages[0].Name != "003" {
		t.Fatalf("forked plan %+v", got)
	}
	m := s.manifest(s.ref("gallery", to))
	if m.Meta["fork"] != nil || m.Meta["forked"] == nil || len(files(m, "originals/")) != 5 || len(files(m, "audio/")) != 1 {
		t.Fatalf("forked manifest %v", m.Meta)
	}
	if f, ok := m.Get("low-res/003.webp"); !ok || f.Blob == "" {
		t.Fatal("outputs were not copied")
	}
	// The source is untouched, and a repeated fork is a no-op.
	if resp, _ := s.call(s.creator, "POST", path, body); resp.StatusCode != 202 {
		t.Fatalf("repeat fork: %d", resp.StatusCode)
	}
	if p := s.plan(s.creator, from, func(*versions.ReaderPlan) bool { return true }); p.Publication != src.Publication {
		t.Fatal("the source was republished")
	}
}

// A page archive uploaded to a version expands into its pages in natural
// order, retaining the source archive for authorized editor inspection.
func TestImport(t *testing.T) {
	s := newSite(t, siteOptions{})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	ref := s.ref("gallery", v)
	archive := zipOf(t, map[string][]byte{"p1.png": pngOf(t, 20, 30, 1), "p10.png": pngOf(t, 20, 30, 2), "p2.png": pngOf(t, 20, 30, 3),
		"__MACOSX/._p1.png": []byte("x"), "notes.txt": []byte("hi")})
	s.mustUpload(s.creator, ref, "import/book", "application/zip", archive)
	p := s.plan(access.Actor{Anonymous: true, IP: "203.0.113.2"}, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 3 })
	var names []string
	for _, pg := range p.Segments[0].Pages {
		names = append(names, pg.Name)
	}
	if strings.Join(names, ",") != "p1,p2,p10" || p.Segments[0].Locked {
		t.Fatalf("imported plan %+v", p)
	}
	if archive, ok := s.manifest(ref).Get("import/book.zip"); !ok || archive.Meta["archive_completed_sha256"] == nil {
		t.Fatal("completed source archive was not retained")
	}
	read, err := s.reader.Read(s.ctx, ref, s.creator, media.ReadOptions{Prefix: "import/", Editor: true})
	if err != nil || len(read.Files) != 1 || read.Files[0].URL == "" {
		t.Fatalf("completed archive has no editor source capability: %+v %v", read, err)
	}
	// An archive of no images fails and stays, publishing nothing new.
	s.mustUpload(s.creator, ref, "import/empty", "application/zip", zipOf(t, map[string][]byte{"a.txt": []byte("a")}))
	eventually(t, "the failed archive", time.Minute, func() bool {
		f, ok := s.manifest(ref).Get("import/empty.zip")
		return ok && f.Fail() != nil
	})
}

func TestImportReservesLegacyPhysicalFilenames(t *testing.T) {
	s := newSite(t, siteOptions{})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	ref := s.ref("gallery", v)
	s.mustUpload(s.creator, ref, "originals/cover", "image/png", pngOf(t, 20, 30, 61))
	s.plan(access.Actor{Anonymous: true, IP: "203.0.113.2"}, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 1 })
	original, _ := s.manifest(ref).Get("originals/cover.png")
	_, err := s.ms.EditExisting(s.ctx, ref, func(m *media.Manifest) error {
		i := m.Find("originals/cover.png")
		page := m.Files[i].Meta["page"].(map[string]any)
		page["name"] = "legacy-label"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	archive := zipOf(t, map[string][]byte{"Cover.png": pngOf(t, 20, 30, 62)})
	s.mustUpload(s.creator, ref, "import/book", "application/zip", archive)
	s.plan(access.Actor{Anonymous: true, IP: "203.0.113.2"}, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 2 })
	m := s.manifest(ref)
	kept, ok := m.Get("originals/cover.png")
	if !ok || kept.Blob != original.Blob {
		t.Fatal("archive overwrote the retained source")
	}
	imported, ok := m.Get("originals/Cover (2).png")
	if !ok {
		t.Fatalf("missing collision-safe imported filename: %+v", m.Files)
	}
	page := imported.Meta["page"].(map[string]any)
	if page["source_name"] != "Cover.png" || page["source_entry"] != "Cover.png" {
		t.Fatalf("lost archive filename provenance: %+v", page)
	}
}

// Reader metadata names pages, anchors and audio the version has; the plan
// keeps what the viewer can read.
func TestReaderMeta(t *testing.T) {
	s := newSite(t, siteOptions{})
	c, v := gallery(t, s)
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Playback != nil })
	// A commit places at most once; publishing copies finished files and
	// places nothing, and nothing feeds back once settled.
	s.settles()
	if n, commits := s.placements(), s.placing(s.ref("gallery", v)); n["gallery"] > commits || n["gallery.published"] > 0 {
		t.Fatalf("placements %v for %d commits: want at most one per commit and none for publication", n, commits)
	}
	path := "/v1/contents/" + c + "/versions/" + v.Version + "/reader"
	page, anchor := sourcePageID(t, s, s.ref("gallery", v), "originals/001.png"), "a1"
	last := sourcePageID(t, s, s.ref("gallery", v), "originals/005.png")
	meta := versions.ReaderMeta{SchemaVersion: 1,
		Anchors:    []versions.Anchor{{ID: "a1", Page: page, OffsetPercent: 50}, {ID: "a5", Page: last, OffsetPercent: 10}},
		Background: []versions.BackgroundCue{{File: "theme", FromPage: &page}},
		Foreground: []versions.ForegroundCue{{File: "theme", Anchor: &anchor}}}
	if resp, _ := s.call(s.stranger, "PUT", path, meta); resp.StatusCode != 403 {
		t.Fatalf("a stranger set reader metadata: %d", resp.StatusCode)
	}
	bad := meta
	bad.Anchors = []versions.Anchor{{ID: "x", Page: "999"}}
	if resp, _ := s.call(s.creator, "PUT", path, bad); resp.StatusCode != 400 {
		t.Fatalf("metadata naming a missing page: %d", resp.StatusCode)
	}
	if resp, b := s.call(s.creator, "PUT", path, meta); resp.StatusCode != 204 {
		t.Fatalf("set reader metadata: %d %s", resp.StatusCode, b)
	}
	editor := s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Reader != nil })
	if len(editor.Reader.Anchors) != 2 || len(editor.Reader.Background) != 1 || len(editor.Reader.Foreground) != 1 {
		t.Fatalf("editor reader %+v", editor.Reader)
	}
	fan := s.plan(s.fan, v, func(p *versions.ReaderPlan) bool { return p.Reader != nil })
	if len(fan.Reader.Anchors) != 1 || fan.Reader.Anchors[0].ID != "a1" || len(fan.Reader.Background) != 0 || len(fan.Reader.Foreground) != 0 {
		t.Fatalf("fan reader %+v", fan.Reader)
	}
}

// Downloads stream a cached archive, or a fresh copy with the pages the
// host's policy names marked, to viewers who open every page, within their
// bucket's quota.
func TestDownload(t *testing.T) {
	s := newSite(t, siteOptions{policy: marker{}, downloads: versions.Downloads{
		User:  []versions.Quota{{Limit: 5, Per: time.Hour}},
		Tiers: []versions.TierQuota{{Tier: "premium", Quotas: []versions.Quota{{Limit: 1, Per: time.Hour}}}}}})
	c := s.content("gallery", `{"level":"tier","tiers":["premium"]}`)
	v := s.version(c)
	ref := s.ref("gallery", v)
	for i, seed := range []uint8{1, 2, 3} {
		s.mustUpload(s.creator, ref, "originals/"+string(rune('a'+i)), "image/png", pngOf(t, 20, 20, seed))
	}
	s.plan(s.creator, v, func(*versions.ReaderPlan) bool { return true })
	if _, ok := s.manifest(ref).Get("download/pages.zip"); ok {
		t.Fatal("reader ZIP was eagerly produced")
	}
	path := "/v1/contents/" + c + "/versions/" + v.Version + "/download"
	if resp, _ := s.call(access.Actor{}, "GET", path, nil); resp.StatusCode != 403 {
		t.Fatalf("anonymous download of a premium version: %d", resp.StatusCode)
	}
	entries := func(b []byte) map[string][]byte {
		t.Helper()
		zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][]byte{}
		for _, f := range zr.File {
			rc, _ := f.Open()
			out[f.Name], _ = io.ReadAll(rc)
			rc.Close()
		}
		return out
	}
	// The policy marks: the download's URL is the archive route, streaming
	// the pages with page 2 marked (9 is past the end).
	resp, b := s.call(s.fan, "GET", path, nil)
	var granted versions.FileDownload
	if err := json.Unmarshal(b, &granted); resp.StatusCode != 200 || err != nil || !strings.HasPrefix(granted.URL, "archive?") || granted.Name != "A Title.zip" {
		t.Fatalf("fan download: %d %s", resp.StatusCode, b)
	}
	archive := strings.TrimSuffix(path, "download") + granted.URL
	resp, b = s.call(access.Actor{}, "GET", archive, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Disposition") == "" || resp.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("marked archive: %d %s", resp.StatusCode, b)
	}
	marked := entries(b)
	// Entries retain filenames; only the marked image's format changes.
	if len(marked) != 3 || !bytes.HasSuffix(marked["b.png"], []byte("marked:"+s.fan.ID)) || marked["a.webp"] == nil ||
		bytes.Contains(marked["c.webp"], []byte("marked:")) {
		t.Fatalf("marked archive %v", keys(marked))
	}
	if resp, _ := s.call(access.Actor{}, "GET", strings.Replace(archive, "for="+s.fan.ID, "for="+s.stranger.ID, 1), nil); resp.StatusCode != 404 {
		t.Fatalf("another viewer's ticket: %d", resp.StatusCode)
	}
	if resp, _ := s.call(s.fan, "GET", path, nil); resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("a download past the premium quota: %d", resp.StatusCode)
	}
	// A version no longer locked behind premium is unmarked: the archive
	// route lazily caches the source bytes, here an editor's in the user bucket.
	s.exec(`UPDATE `+s.table("content_items")+` SET access = '{"level":"public"}' WHERE tenant_id = $1 AND id = $2`, s.tenant, c)
	d, err := s.svc.Download(s.ctx, s.creator, v)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "A Title.zip" || d.Size != 0 || !strings.HasPrefix(d.URL, "archive?") || !strings.Contains(d.URL, "revision=") {
		t.Fatalf("lazy archive %+v", d)
	}
	s.exec(`INSERT INTO `+s.table("content_segments")+` (tenant_id, version_id, position, start_index, end_index, release_at)
VALUES ($1, $2, 1, 2, 3, now() + interval '1 hour')`, s.tenant, v.Version)
	if _, err := s.svc.Download(s.ctx, s.fan, v); !errors.Is(err, versions.ErrForbidden) {
		t.Fatalf("a download with a segment unreleased: %v", err)
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
