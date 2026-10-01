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
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

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

// Private is all or nothing: a viewer who can see the item but has no
// access gets no URL, token or cookie, and each file as its path, type and
// size only. Nothing in the answer names a blob. The item's previews are
// public, so the same viewer gets those.
func TestReadWithoutAccess(t *testing.T) {
	f := newFixtureOn(t, s3test.Open(t), func(c *media.Config) {
		c.Kinds[0].Public = append(c.Kinds[0].Public, media.Public{Name: "preview", From: "originals/{name}", To: "preview-{n}.webp", First: 2})
	})
	f.visible(1)
	g := f.gallery(1, 3)
	ctx := context.Background()
	item, _ := f.reg.Item(g)
	anon := access.Actor{Anonymous: true}
	want := []string{"https://" + mediaHost + "/v1/" + item.PublicPrefix() + "preview-1.webp", "https://" + mediaHost + "/v1/" + item.PublicPrefix() + "preview-2.webp"}
	full, err := f.rd.Read(ctx, g, anon, media.ReadOptions{Prefix: "high/"})
	if err != nil || full.Access != media.AccessFull || !slices.Equal(full.Previews, want) || full.Files[0].URL == "" {
		t.Fatalf("with access: %+v %v", full, err)
	}
	f.res.set(cid(1), access.Resolution{Visible: true})
	rd, err := media.NewReader(media.ReaderOptions{Manifests: f.ms, Delivery: media.Delivery{Mode: media.DeliverCookie, CookieDomain: "doujins.test", SigningKey: signKey}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*media.Reader{f.rd, rd} {
		res, err := r.Read(ctx, g, anon, media.ReadOptions{Download: true})
		if err != nil || res.Access != media.AccessNone || res.Cookie != nil || len(res.HLS) != 0 || res.Total != 7 || !slices.Equal(res.Previews, want) {
			t.Fatalf("without access: %+v %v", res, err)
		}
		if fi := res.Files[0]; !reflect.DeepEqual(fi, media.FileInfo{Path: "thumb/000.webp", Type: "image/png", Size: int64(len(png(100))), Locked: true}) {
			t.Fatalf("a locked file is its path, type and size: %+v", fi)
		}
		body, _ := json.Marshal(res)
		if strings.Contains(string(body), "sha256-") || strings.Contains(string(body), "?t=") || strings.Contains(string(body), "/private/") {
			t.Fatalf("a read without access names a blob or a token: %s", body)
		}
		grant, err := r.Grant(ctx, g, anon)
		if err != nil || grant.Full() || grant.Cookie() != nil {
			t.Fatalf("grant without access: %+v %v", grant, err)
		}
		for _, file := range grant.Manifest.Files {
			if _, err := grant.URL(file, false); !errors.Is(err, media.ErrNotAllowed) {
				t.Fatalf("%s signed without access: %v", file.Path, err)
			}
		}
	}
	// The previews are served to anyone; a private file is not.
	if status, body, _ := f.fetch(want[0]); status != http.StatusOK || body != string(png(100)) {
		t.Fatalf("preview served %d %q", status, body)
	}
	if status, _, _ := f.fetch(strings.Split(full.Files[0].URL, "?")[0]); status != http.StatusNotFound {
		t.Fatalf("a private file without a token: %d", status)
	}
}

// Viewers of served originals get what a file is and its URL, never editor
// fields (audit): no edit, meta, pending work, failure, or a frame's source
// blob. Editors still get them.
func TestViewerReadsCarryNoEditorFields(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	post := f.ref("post", 1)
	ctx := context.Background()
	p, blob := f.upload(post, "inline/x.png", "image/png", png(1))
	f.commit(post, media.Op{Op: media.OpPut, Path: p, Blob: blob, Meta: map[string]any{"alt": "private note"}, Edit: &media.Edit{Rotate: 90}})
	if _, err := f.ms.EditExisting(ctx, post, func(m *media.Manifest) error {
		i := m.Find(p)
		m.Files[i].Frame = &media.Frame{Of: blobOf([]byte("an unpublished video"))}
		m.SetFailed(p, errors.New("render /tmp/worker/scratch failed"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := f.rd.Read(ctx, post, access.Actor{Anonymous: true}, media.ReadOptions{Editor: true})
	if err != nil || len(res.Files) != 1 || res.Files[0].URL == "" {
		t.Fatalf("viewer read %+v %v", res, err)
	}
	if fi := res.Files[0]; fi.Edit != nil || fi.Meta != nil || fi.Frame != nil || fi.Failed != nil || fi.Pending != nil || fi.Upload || fi.EditorURL != "" {
		t.Fatalf("a viewer got editor fields: %+v", fi)
	}
	res, err = f.rd.Read(ctx, post, f.editor, media.ReadOptions{Editor: true})
	if fi := res.Files[0]; err != nil || fi.Edit == nil || fi.Meta["alt"] != "private note" || fi.Frame == nil || fi.Failed == nil || !fi.Upload {
		t.Fatalf("editor read %+v %v", fi, err)
	}
	// Without access the file is listed locked: its path, type and size,
	// and no blob name, source or editor field.
	f.res.set(cid(1), access.Resolution{Visible: true})
	res, err = f.rd.Read(ctx, post, access.Actor{Anonymous: true}, media.ReadOptions{Editor: true, Download: true})
	if err != nil || len(res.Files) != 1 || !reflect.DeepEqual(res.Files[0], media.FileInfo{Path: p, Type: "image/png", Size: int64(len(png(1))), Locked: true}) {
		t.Fatalf("locked listing %+v %v", res.Files, err)
	}
	if body, _ := json.Marshal(res); strings.Contains(string(body), "sha256-") {
		t.Fatalf("a locked listing names a blob: %s", body)
	}
}

// ServeOriginals lists and serves uploads; a download read adds each file's
// download name to its URL, which the agent sends as the attachment name.
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
	if err != nil || len(res.Files) != 1 || res.Files[0].Download != "Café Book.zip" || !strings.Contains(res.Files[0].URL, "dl=") {
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
	f := newFixtureOn(t, s3test.Open(t), func(c *media.Config) { c.Kinds[0].ServeOriginals = true })
	f.visible(1)
	f.gallery(1, 1)
	rd, err := media.NewReader(media.ReaderOptions{Manifests: f.ms,
		Delivery: media.Delivery{Mode: media.DeliverCookie, CookieDomain: "doujins.test", SigningKey: signKey}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rd.Handler(media.HandlerOptions{Identity: identity{f.editor}, Limit: media.RateLimit{Disabled: true}}))
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
	// A download is the same URL with a name: the cookie authorizes it.
	dl, err := rd.Read(context.Background(), f.ref("gallery", 1), f.editor, media.ReadOptions{Prefix: "download/", Download: true})
	if err != nil || len(dl.Files) != 1 || !strings.Contains(dl.Files[0].URL, "?dl=") || strings.Contains(dl.Files[0].URL, "t=") {
		t.Fatalf("cookie download read %+v %v", dl, err)
	}
}

// An item's token opens every private file of the item, in both delivery
// modes: ServeOriginals and HostOnly decide what a generic read lists, not
// what the token reaches. A HostOnly file's URL comes from Grant.HostURL,
// with the item token, and only with access.
func TestItemTokenIsAllOrNothing(t *testing.T) {
	f := newFixtureOn(t, s3test.Open(t), func(c *media.Config) { c.Kinds[0].Private[2].HostOnly = true })
	f.visible(1)
	ref := f.gallery(1, 1)
	ctx := context.Background()
	item, _ := f.reg.Item(ref)
	m, _, _ := f.ms.Get(ctx, ref)
	original, _ := m.Get("originals/000.png")
	archive, _ := m.Get("download/pages.zip")
	for _, mode := range []media.DeliveryMode{media.DeliverCookie, media.DeliverURL} {
		rd, err := media.NewReader(media.ReaderOptions{Manifests: f.ms, Delivery: media.Delivery{Mode: mode, CookieDomain: "doujins.test", SigningKey: signKey}})
		if err != nil {
			t.Fatal(err)
		}
		f.visible(1)
		read, err := rd.Read(ctx, ref, f.editor, media.ReadOptions{})
		if err != nil || (read.Cookie != nil) != (mode == media.DeliverCookie) || len(read.Files) != 2 || read.Files[0].URL == "" {
			t.Fatalf("%s read lists the page's outputs only: %+v %v", mode, read, err)
		}
		var hdr []string
		if read.Cookie != nil {
			hdr = []string{"Cookie", media.CookieName + "=" + read.Cookie.Value}
		}
		u, _ := url.Parse(read.Files[0].URL)
		for _, file := range []media.File{original, archive} {
			key, _ := item.Blob(file.Blob)
			u.Path = "/v1/" + key
			if status, _, _ := f.fetch(u.String(), hdr...); status != http.StatusOK {
				t.Fatalf("%s: the item token did not open %s: %d", mode, file.Path, status)
			}
		}
		g, err := rd.Grant(ctx, ref, f.editor)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := g.URL(archive, true); !errors.Is(err, media.ErrNotAllowed) {
			t.Fatalf("%s: a generic read's URL for a HostOnly file: %v", mode, err)
		}
		download, err := g.HostURL(archive.Path, true)
		if err != nil || !strings.Contains(download, "t=") {
			t.Fatalf("%s host URL %q %v", mode, download, err)
		}
		if status, _, hdr := f.fetch(download); status != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Disposition"), "attachment") {
			t.Fatalf("%s host archive: %d %v", mode, status, hdr)
		}
		if _, err := g.HostURL("high/000.webp", false); !errors.Is(err, media.ErrNotAllowed) {
			t.Fatalf("%s: HostURL for a listed preset: %v", mode, err)
		}
		f.res.set(cid(1), access.Resolution{Visible: true})
		none, err := rd.Grant(ctx, ref, f.editor)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := none.HostURL(archive.Path, true); !errors.Is(err, media.ErrNotAllowed) {
			t.Fatalf("%s host archive without access: %v", mode, err)
		}
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
	srv := httptest.NewServer(f.rd.Handler(media.HandlerOptions{Identity: identity{f.editor}, Limit: media.RateLimit{Disabled: true}}))
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
	// Without access there is no playlist.
	f.res.set(cid(1), access.Resolution{Visible: true})
	resp, _ = http.Get(srv.URL + "/video/" + cid(1) + "/hls/hls/master.m3u8")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a playlist without access: %d", resp.StatusCode)
	}
}

// The read handler maps errors and rate limits per viewer.
func TestReadHandler(t *testing.T) {
	f := newFixtureOn(t, s3test.Open(t), nil)
	f.visible(1)
	f.gallery(1, 1)
	srv := httptest.NewServer(f.rd.Handler(media.HandlerOptions{Identity: identity{f.editor}, Limit: media.RateLimit{PerSecond: 1, Burst: 2}}))
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

// The read API answers 429 with Retry-After once a viewer has opened too
// many items this hour: reads and playlists alike, the same item again is
// free, and neither a viewer without access nor an exempt one is counted.
func TestIssuanceLimitOnReads(t *testing.T) {
	f := newFixture(t)
	viewer := access.Actor{ID: "viewer", Kind: "user"}
	for n := 1; n <= 4; n++ {
		f.res.set(cid(n), access.Resolution{Visible: true, Accessible: true})
	}
	f.res.set(cid(5), access.Resolution{Visible: true})
	actor := viewer
	rd, err := media.NewReader(media.ReaderOptions{Manifests: f.ms, Delivery: media.Delivery{Mode: media.DeliverURL, SigningKey: signKey},
		Issuance: media.Issuance{PerHour: 2, Exempt: func(a access.Actor) bool { return a.Kind == "staff" }}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rd.Handler(media.HandlerOptions{Identity: identityFunc(func() access.Actor { return actor }), Limit: media.RateLimit{Disabled: true}}))
	defer srv.Close()
	get := func(path string) (*http.Response, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	for _, n := range []int{5, 1, 2, 1, 5} { // 5 gives no access: not counted
		if resp, body := get("/gallery/" + cid(n)); resp.StatusCode != http.StatusOK {
			t.Fatalf("item %d: %d %s", n, resp.StatusCode, body)
		}
	}
	for _, path := range []string{"/gallery/" + cid(3), "/video/" + cid(4) + "/hls/hls/master.m3u8"} {
		resp, body := get(path)
		wait, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		if resp.StatusCode != http.StatusTooManyRequests || wait < 1 || wait > 7200 || !strings.Contains(body, `"rate_limited"`) || strings.Contains(body, "?t=") {
			t.Fatalf("%s over the limit: %d retry-after %q %s", path, resp.StatusCode, resp.Header.Get("Retry-After"), body)
		}
	}
	if resp, _ := get("/gallery/" + cid(2)); resp.StatusCode != http.StatusOK {
		t.Fatalf("an item opened this hour: %d", resp.StatusCode)
	}
	actor = access.Actor{ID: "staff", Kind: "staff"}
	for n := 1; n <= 4; n++ {
		if resp, _ := get("/gallery/" + cid(n)); resp.StatusCode != http.StatusOK {
			t.Fatalf("an exempt actor, item %d: %d", n, resp.StatusCode)
		}
	}
	if _, err := rd.Grant(context.Background(), f.ref("gallery", 3), viewer); !errors.Is(err, media.ErrRateLimited) {
		t.Fatalf("Grant over the limit: %v", err)
	}
}

type identityFunc func() access.Actor

func (f identityFunc) Actor(context.Context) (access.Actor, bool) { return f(), true }

// redisURLs lists CONTENTKIT_TEST_REDIS_URLS (Redis and Garnet in CI); unset skips.
func redisURLs(t *testing.T) []string {
	var out []string
	for _, u := range strings.Split(os.Getenv("CONTENTKIT_TEST_REDIS_URLS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		t.Skip("CONTENTKIT_TEST_REDIS_URLS not set")
	}
	return out
}

// Replicas sharing one Redis (or Garnet) enforce one issuance limit per
// viewer, in expiring, prefixed keys; a refused item is not kept. When
// Redis is unreachable each replica falls back to its own count.
func TestSharedIssuanceLimit(t *testing.T) {
	for _, u := range redisURLs(t) {
		t.Run(u, func(t *testing.T) {
			f := newFixture(t)
			opt, err := redis.ParseURL(u)
			if err != nil {
				t.Fatal(err)
			}
			rdb := redis.NewClient(opt)
			t.Cleanup(func() { rdb.Close() })
			ctx := context.Background()
			if err := rdb.Ping(ctx).Err(); err != nil {
				t.Fatal(err)
			}
			prefix := "cktest:" + uuid.NewString() + ":"
			replica := func(c redis.UniversalClient) *media.Reader {
				rd, err := media.NewReader(media.ReaderOptions{Manifests: f.ms, Delivery: media.Delivery{Mode: media.DeliverURL, SigningKey: signKey},
					Issuance: media.Issuance{PerHour: 3, Redis: c, KeyPrefix: prefix}})
				if err != nil {
					t.Fatal(err)
				}
				return rd
			}
			a, b := replica(rdb), replica(rdb)
			viewer := access.Actor{ID: "scraper", Kind: "user"}
			for n := 1; n <= 6; n++ {
				f.res.set(cid(n), access.Resolution{Visible: true, Accessible: true})
			}
			open := func(rd *media.Reader, who access.Actor, n int) error {
				_, err := rd.Grant(ctx, f.ref("gallery", n), who)
				return err
			}
			before := media.RedisErrors.Value()
			for i, rd := range []*media.Reader{a, b, a, b} { // items 1, 2, 3, then 1 again
				if err := open(rd, viewer, i%3+1); err != nil {
					t.Fatalf("open %d: %v", i, err)
				}
			}
			for _, rd := range []*media.Reader{a, b} {
				var le *media.LimitError
				if err := open(rd, viewer, 4); !errors.As(err, &le) || le.RetryAfter <= 0 || le.RetryAfter > 2*time.Hour {
					t.Fatalf("a fourth item on a replica: %v", err)
				}
			}
			if err := open(b, access.Actor{ID: "someone-else", Kind: "user"}, 4); err != nil {
				t.Fatalf("another viewer: %v", err)
			}
			if got := media.RedisErrors.Value(); got != before {
				t.Fatalf("redis errors %d -> %d", before, got)
			}
			keys, err := rdb.Keys(ctx, prefix+"*scraper*").Result()
			if err != nil || len(keys) != 1 {
				t.Fatalf("keys under %q: %v %v", prefix, keys, err)
			}
			if n, err := rdb.SCard(ctx, keys[0]).Result(); err != nil || n != 3 {
				t.Fatalf("items kept for the viewer: %d %v", n, err)
			}
			if ttl, err := rdb.PTTL(ctx, keys[0]).Result(); err != nil || ttl <= 0 || ttl > 2*time.Hour+2*time.Second {
				t.Fatalf("%s ttl %v %v", keys[0], ttl, err)
			}
			// Redis down: the replica counts on its own, and says so.
			dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
			t.Cleanup(func() { dead.Close() })
			alone := replica(dead)
			for n := 1; n <= 3; n++ {
				if err := open(alone, viewer, n); err != nil {
					t.Fatalf("with Redis down, item %d: %v", n, err)
				}
			}
			if err := open(alone, viewer, 5); !errors.Is(err, media.ErrRateLimited) {
				t.Fatalf("with Redis down, a fourth item: %v", err)
			}
			if got := media.RedisErrors.Value(); got == before {
				t.Fatal("the Redis failure was not counted")
			}
		})
	}
}
