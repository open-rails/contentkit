package media_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

// gallery commits n pages and a cover to gallery item id and produces them.
func (f *fixture) gallery(id, n int) contentref.ContentRef {
	f.t.Helper()
	g := f.ref("gallery", id)
	for i := range n {
		f.put(g, fmt.Sprintf("originals/%03d.png", i), "image/png", png(id*100+i))
	}
	f.put(g, "cover.png", "image/png", png(id*100+99))
	f.produce(g)
	return g
}

func urls(res *media.ReadResult) map[string]string {
	out := map[string]string{}
	for _, f := range res.Files {
		out[f.Path] = f.URL
	}
	return out
}

// A full-access read lists the derived files (not the unserved originals)
// in manifest order with URLs the access agent serves; prefix, offset and
// limit select them.
func TestReadFullAccess(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.gallery(1, 3)
	ctx := context.Background()
	res, err := f.rd.Read(ctx, g, f.editor, media.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Access != media.AccessFull || res.Total != 7 || res.Files[0].Path != "thumb/000.webp" || res.Files[5].Path != "high/002.webp" ||
		res.Files[6].Path != "download/pages.zip" {
		t.Fatalf("read %+v", res)
	}
	for _, fi := range res.Files {
		if fi.URL == "" || fi.Locked || strings.HasPrefix(fi.Path, "originals/") {
			t.Fatalf("file %+v", fi)
		}
	}
	status, body, hdr := f.fetch(res.Files[0].URL)
	if status != http.StatusOK || body != string(png(100)) || hdr.Get("Cache-Control") != "private, max-age=31536000, immutable" {
		t.Fatalf("agent served %d %q %v", status, body, hdr)
	}
	page, err := f.rd.Read(ctx, g, f.editor, media.ReadOptions{Prefix: "high/", Offset: 1, Limit: 1})
	if err != nil || page.Total != 3 || len(page.Files) != 3 || page.Files[0].URL != "" || page.Files[1].URL == "" || page.Files[2].URL != "" {
		t.Fatalf("page %+v %v", page, err)
	}
	// Another item's token does not open this item's blobs.
	f.visible(2)
	other := f.gallery(2, 1)
	res2, _ := f.rd.Read(ctx, other, f.editor, media.ReadOptions{})
	tok := res2.Files[0].URL[strings.Index(res2.Files[0].URL, "?t="):]
	plain := strings.Split(res.Files[0].URL, "?")[0]
	if status, _, _ := f.fetch(plain + tok); status != http.StatusNotFound {
		t.Fatalf("another item's token: %d", status)
	}
	if _, err := f.rd.Read(ctx, f.ref("gallery", 3), f.editor, media.ReadOptions{}); !errors.Is(err, media.ErrNotVisible) {
		t.Fatalf("invisible item: %v", err)
	}
}

// Preview access serves the first PreviewLimit pages' outputs; a teaser
// page's outputs need only visibility; the zip needs full access.
func TestReadPreviewAndTeaser(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.gallery(1, 4)
	ctx := context.Background()
	f.commit(g, media.Op{Op: media.OpPut, Path: "originals/003.png", Blob: blobOf(png(103)), Meta: map[string]any{"teaser": true}})
	f.produce(g)
	f.res.set(cid(1), access.Resolution{Visible: true, PreviewLimit: 2})
	res, err := f.rd.Read(ctx, g, access.Actor{Anonymous: true}, media.ReadOptions{Prefix: "high/"})
	if err != nil {
		t.Fatal(err)
	}
	got := urls(res)
	if res.Access != media.AccessPreview || res.PreviewLimit != 2 || got["high/000.webp"] == "" || got["high/001.webp"] == "" ||
		got["high/002.webp"] != "" || got["high/003.webp"] == "" {
		t.Fatalf("preview %+v", res)
	}
	if !res.Files[2].Locked || !res.Files[3].Teaser {
		t.Fatalf("locked and teaser flags %+v", res.Files)
	}
	f.res.set(cid(1), access.Resolution{Visible: true})
	res, _ = f.rd.Read(ctx, g, access.Actor{Anonymous: true}, media.ReadOptions{Prefix: "high/"})
	if got := urls(res); res.Access != media.AccessNone || got["high/000.webp"] != "" || got["high/003.webp"] == "" {
		t.Fatalf("no access %+v", res)
	}
}

// ServeOriginals lists and serves uploads; a download read signs each
// file's download name, which the agent sends as the attachment name.
func TestReadOriginalsAndDownloads(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	post := f.ref("post", 1)
	p, blob := f.upload(post, "inline/x.png", "image/png", png(1))
	f.commit(post, media.Op{Op: media.OpPut, Path: p, Blob: blob})
	ctx := context.Background()
	res, err := f.rd.Read(ctx, post, f.editor, media.ReadOptions{})
	if err != nil || len(res.Files) != 1 || res.Files[0].Path != p || res.Files[0].URL == "" {
		t.Fatalf("served original %+v %v", res, err)
	}

	f.visible(2)
	g := f.gallery(2, 1)
	if _, err := f.ms.EditExisting(ctx, g, func(m *media.Manifest) error {
		item, _ := f.reg.Item(g)
		zip := []byte("zip bytes")
		key, _ := item.Blob(blobOf(zip))
		if _, err := f.env.Store.Put(ctx, key, bytes.NewReader(zip), int64(len(zip)), media.PutOptions{ContentType: "application/zip"}); err != nil {
			return err
		}
		m.Meta = map[string]any{"title": "Café Book"}
		return m.SetOutputs("high/", "zip", []media.File{{Path: "download/pages.zip", Blob: blobOf(zip), Type: "application/zip", Size: int64(len(zip)), FP: "x"}})
	}); err != nil {
		t.Fatal(err)
	}
	res, err = f.rd.Read(ctx, g, f.editor, media.ReadOptions{Prefix: "download/", Download: true})
	if err != nil || len(res.Files) != 1 || res.Files[0].Download != "Café Book.zip" || !strings.Contains(res.Files[0].URL, "&dl=") {
		t.Fatalf("download read %+v %v", res, err)
	}
	status, body, hdr := f.fetch(res.Files[0].URL)
	if status != http.StatusOK || body != "zip bytes" || !strings.Contains(hdr.Get("Content-Disposition"), "filename*=UTF-8''Caf%C3%A9%20Book.zip") {
		t.Fatalf("download %d %q %v", status, body, hdr)
	}
}

// Cookie delivery returns plain URLs and the item cookie, which the agent
// accepts for every private file of the item.
func TestReadCookieDelivery(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	f.gallery(1, 1)
	rd, err := media.NewReader(media.ReaderOptions{Manifests: f.ms,
		Delivery: media.Delivery{Mode: media.DeliverCookie, CookieDomain: "doujins.test", SigningKey: signKey}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rd.Handler(media.HandlerOptions{Identity: identity{f.editor}, Limit: media.ViewerLimit{Disabled: true}}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/gallery/" + cid(1))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var res media.ReadResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Name != media.CookieName || cookies[0].Path != "/v1/"+f.ns+"/gallery/"+cid(1)+"/private/" ||
		strings.Contains(res.Files[0].URL, "?") {
		t.Fatalf("cookie %+v url %s", cookies, res.Files[0].URL)
	}
	if status, _, _ := f.fetch(res.Files[0].URL); status != http.StatusNotFound {
		t.Fatalf("no cookie: %d", status)
	}
	if status, body, _ := f.fetch(res.Files[0].URL, "Cookie", media.CookieName+"="+cookies[0].Value); status != http.StatusOK || body != string(png(100)) {
		t.Fatalf("with cookie: %d %q", status, body)
	}
}

type identity struct{ a access.Actor }

func (i identity) Actor(context.Context) (access.Actor, bool) { return i.a, true }

// An editor read lists the uploads with their editing state; missing editor
// views are asked of the worker and served once rendered.
func TestReadEditor(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.ref("gallery", 1)
	f.put(g, "cover.png", "image/png", png(1))
	ctx := context.Background()
	f.q.take()
	res, err := f.rd.Read(ctx, g, f.editor, media.ReadOptions{Editor: true})
	if err != nil || res.State != media.StateProcessing || len(res.Files) != 1 || !res.Files[0].Upload || res.Files[0].EditorURL != "" {
		t.Fatalf("editor read %+v %v", res, err)
	}
	if jobs := f.q.take(); len(jobs) != 1 || !jobs[0].Editor {
		t.Fatalf("editor views not asked for: %+v", jobs)
	}
	m, _, _ := f.ms.Get(ctx, g)
	cover, _ := m.Get("cover.png")
	item, _ := f.reg.Item(g)
	key, _ := item.Blob(f.reg.EditorView(cover))
	if _, err := f.env.Store.Put(ctx, key, strings.NewReader("view"), 4, media.PutOptions{ContentType: "image/webp"}); err != nil {
		t.Fatal(err)
	}
	f.produce(g)
	res, _ = f.rd.Read(ctx, g, f.editor, media.ReadOptions{Editor: true})
	if res.State != media.StateReady || res.Files[0].EditorURL == "" {
		t.Fatalf("editor view %+v", res.Files[0])
	}
	if status, body, _ := f.fetch(res.Files[0].EditorURL); status != http.StatusOK || body != "view" {
		t.Fatalf("editor view served %d %q", status, body)
	}
	// Non-editors never get the editing state.
	f.res.set(cid(1), access.Resolution{Visible: true, Accessible: true})
	res, _ = f.rd.Read(ctx, g, f.editor, media.ReadOptions{Editor: true})
	if res.State != "" || len(res.Files) != 0 {
		t.Fatalf("a viewer's editor read %+v", res)
	}
}

// HLS playlists are built per request from the manifest's track files and
// their index blobs, with sidecar subtitles; every URI is signed.
func TestHLSPlaylists(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	v := f.ref("video", 1)
	f.put(v, "source.mp4", "video/mp4", []byte("video"))
	sub, blob := f.upload(v, "subs/en.srt", "application/x-subrip", []byte("1\n00:00:01,000 --> 00:00:02,000\nhi\n"))
	f.commit(v, media.Op{Op: media.OpPut, Path: sub, Blob: blob, Meta: map[string]any{"lang": "en", "label": "English"}})
	ctx := context.Background()
	item, _ := f.reg.Item(v)
	put := func(body string) string {
		b := []byte(body)
		key, _ := item.Blob(blobOf(b))
		if _, err := f.env.Store.Put(ctx, key, bytes.NewReader(b), int64(len(b)), media.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		return blobOf(b)
	}
	index := func(idx media.TrackIndex) string {
		b, _ := json.Marshal(idx)
		return put(string(b))
	}
	segs := media.TrackIndex{Segments: []media.Segment{{Offset: 100, Length: 50, Seconds: 4}, {Offset: 150, Length: 40, Seconds: 2.5}}}
	if _, err := f.ms.EditExisting(ctx, v, func(m *media.Manifest) error {
		if i := m.Find("source.mp4"); i >= 0 {
			m.Files[i].Dur = 6.5
		}
		if err := m.SetOutputs("source.mp4", "hls", []media.File{
			{Path: "hls/1080-h264.mp4", Blob: put("v1080"), Type: "video/mp4", W: 1920, H: 1080,
				Track: &media.Track{Kind: media.TrackVideo, Codec: "h264", Codecs: "avc1.640028", Bandwidth: 5000000, Index: index(segs)}},
			{Path: "hls/480-h264.mp4", Blob: put("v480"), Type: "video/mp4", W: 854, H: 480,
				Track: &media.Track{Kind: media.TrackVideo, Codec: "h264", Codecs: "avc1.64001e", Bandwidth: 1000000, Index: index(segs)}},
			{Path: "hls/audio-a1.mp4", Blob: put("a1"), Type: "audio/mp4",
				Track: &media.Track{Kind: media.TrackAudio, ID: "a1", Lang: "ja", Default: true, Bandwidth: 128000, Codecs: "mp4a.40.2", Index: index(segs)}},
			{Path: "hls/sprite.jpg", Blob: put("sprite"), Type: "image/jpeg",
				Track: &media.Track{Kind: media.TrackSprite, Index: index(media.TrackIndex{Sprite: &media.Sprite{Cols: 2, Rows: 1, W: 160, H: 90, Interval: 5}})}},
		}); err != nil {
			return err
		}
		return m.SetOutputs(sub, "vtt", []media.File{{Path: "vtt/en.vtt", Blob: put("WEBVTT\n"), Type: "text/vtt"}})
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(f.rd.Handler(media.HandlerOptions{Identity: identity{f.editor}, Limit: media.ViewerLimit{Disabled: true}}))
	defer srv.Close()
	res, err := f.rd.Read(ctx, v, f.editor, media.ReadOptions{})
	if err != nil || len(res.HLS) != 1 || res.HLS[0] != "hls/" {
		t.Fatalf("hls dirs %+v %v", res.HLS, err)
	}
	get := func(p string) string {
		t.Helper()
		resp, err := http.Get(srv.URL + "/video/" + cid(1) + "/hls/" + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, b)
		}
		return string(b)
	}
	master := get("hls/master.m3u8")
	for _, want := range []string{"RESOLUTION=1920x1080", "../hls/480-h264.mp4.m3u8", `LANGUAGE="ja"`, `NAME="English"`, "../vtt/en.vtt.m3u8", "BANDWIDTH=5128000"} {
		if !strings.Contains(master, want) {
			t.Fatalf("master lacks %q:\n%s", want, master)
		}
	}
	if i, j := strings.Index(master, "1080-h264"), strings.Index(master, "480-h264"); i < 0 || i > j {
		t.Fatalf("the start variant (up to 1080) is not first:\n%s", master)
	}
	media1080 := get("hls/1080-h264.mp4.m3u8")
	if !strings.Contains(media1080, `#EXT-X-MAP:URI="https://`+mediaHost) || !strings.Contains(media1080, "#EXT-X-BYTERANGE:40@150") {
		t.Fatalf("media playlist:\n%s", media1080)
	}
	u := media1080[strings.Index(media1080, "https://"):]
	u = u[:strings.IndexAny(u, "\"\n")]
	if status, body, _ := f.fetch(u); status != http.StatusOK || body != "v1080" {
		t.Fatalf("segment blob %d %q", status, body)
	}
	if subs := get("vtt/en.vtt.m3u8"); !strings.Contains(subs, "#EXTINF:6.500,") {
		t.Fatalf("subtitle playlist:\n%s", subs)
	}
	if sprite := get("hls/sprite.vtt"); !strings.Contains(sprite, "#xywh=160,0,160,90") || !strings.Contains(sprite, "00:00:05.000 --> 00:00:06.500") {
		t.Fatalf("sprite:\n%s", sprite)
	}
	resp, _ := http.Get(srv.URL + "/video/" + cid(1) + "/hls/source.mp4.m3u8")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an unserved upload's playlist: %d", resp.StatusCode)
	}
}

// The read handler maps errors and rate limits per viewer.
func TestReadHandler(t *testing.T) {
	f := newFixtureOn(t, s3test.Open(t), nil)
	f.visible(1)
	f.gallery(1, 1)
	srv := httptest.NewServer(f.rd.Handler(media.HandlerOptions{Identity: identity{f.editor}, Limit: media.ViewerLimit{PerSecond: 1, Burst: 2}}))
	defer srv.Close()
	status := func(p string) int {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if s := status("/gallery/" + cid(1) + "?prefix=" + url.QueryEscape("thumb/")); s != http.StatusOK {
		t.Fatalf("read %d", s)
	}
	if s := status("/nope/" + cid(1)); s != http.StatusNotFound {
		t.Fatalf("unknown kind %d", s)
	}
	if s := status("/gallery/" + cid(1)); s != http.StatusTooManyRequests {
		t.Fatalf("burst exceeded %d", s)
	}
}
