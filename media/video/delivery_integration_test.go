package video_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/accessworker"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/video"
)

type verdict struct {
	mu  sync.Mutex
	res access.Resolution
}

func (v *verdict) set(r access.Resolution) { v.mu.Lock(); v.res = r; v.mu.Unlock() }

func (v *verdict) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := map[contentref.ContentKey]access.Resolution{}
	for _, ref := range refs {
		out[ref.Key()] = v.res
	}
	return out, nil
}

// delivery serves the read API (host) and the access worker over TLS, as
// the site and media hosts; one client with a cookie jar plays the browser.
type delivery struct {
	e       *env
	verdict *verdict
	worker  *httptest.Server
	api     *httptest.Server
	client  *http.Client
	bare    *http.Client
	blobs   map[string][]byte
}

var deliveryKey = token.Key{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}

func newDelivery(t *testing.T, e *env, mode media.DeliveryMode) *delivery {
	t.Helper()
	ring, err := token.NewRing(deliveryKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := accessworker.New(accessworker.Config{Endpoint: e.Config.Endpoint, Bucket: e.Config.Bucket, Region: e.Config.Region,
		AccessKeyID: e.Config.AccessKeyID, SecretAccessKey: e.Config.SecretAccessKey, Ring: ring})
	if err != nil {
		t.Fatal(err)
	}
	d := &delivery{e: e, verdict: &verdict{res: access.Resolution{Visible: true, Accessible: true}}, blobs: map[string][]byte{}}
	d.worker = httptest.NewTLSServer(h) // the cookie is Secure
	t.Cleanup(d.worker.Close)
	host, _ := url.Parse(d.worker.URL)
	r, err := media.NewReader(media.ReaderOptions{Manifests: e.manifests, Kinds: e.kinds, Resolver: d.verdict,
		Delivery: media.Delivery{Mode: mode, BaseURL: d.worker.URL, CookieDomain: host.Hostname(), SigningKey: deliveryKey},
		Hooks: media.Hooks{DownloadName: func(_ context.Context, _ contentref.ContentRef, key string, _ media.Download) (string, error) {
			_, quality, _ := strings.Cut(key, "-")
			return "Title (" + quality + ").mp4", nil
		}}})
	if err != nil {
		t.Fatal(err)
	}
	d.api = httptest.NewTLSServer(http.StripPrefix("/media", r.Handler(media.HandlerOptions{Tenant: e.Tenant})))
	t.Cleanup(d.api.Close)
	jar, _ := cookiejar.New(nil)
	d.client = &http.Client{Transport: d.worker.Client().Transport, Jar: jar}
	d.bare = d.worker.Client()
	return d
}

func (d *delivery) url(path string) string {
	return d.api.URL + "/media/video/" + cid(88) + "@v1/" + path
}

type response struct {
	status int
	body   []byte
	header http.Header
}

func get(t *testing.T, c *http.Client, u string, rng ...int64) response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	if len(rng) == 2 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", rng[0], rng[0]+rng[1]-1))
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{resp.StatusCode, b, resp.Header}
}

// playlist fetches an HLS playlist or VTT from the read API.
func (d *delivery) playlist(t *testing.T, u, contentType string) string {
	t.Helper()
	r := get(t, d.client, u)
	if r.status != 200 || r.header.Get("Content-Type") != contentType || r.header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("%s: %d %v %s", u, r.status, r.header, r.body)
	}
	if !strings.HasPrefix(string(r.body), "#EXTM3U\n") && !strings.HasPrefix(string(r.body), "WEBVTT\n") {
		t.Fatalf("%s: %s", u, r.body)
	}
	return string(r.body)
}

func (d *delivery) blob(t *testing.T, name string) []byte {
	t.Helper()
	if b, ok := d.blobs[name]; ok {
		return b
	}
	b, err := os.ReadFile(d.e.blob(t, name))
	if err != nil {
		t.Fatal(err)
	}
	d.blobs[name] = b
	return b
}

// attrs parses an attribute list; quoted values may hold commas.
func attrs(t *testing.T, s string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for s != "" {
		k, rest, ok := strings.Cut(s, "=")
		if !ok {
			t.Fatalf("attribute list %q", s)
		}
		var v string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			v, rest = rest[1:end+1], rest[end+2:]
		} else {
			v, rest, _ = strings.Cut(rest, ",")
			rest = "," + rest
		}
		out[k] = v
		s = strings.TrimPrefix(rest, ",")
	}
	return out
}

type master struct {
	media    []map[string]string
	variants []map[string]string // with "URI" set to the playlist line
}

func parseMaster(t *testing.T, body string) master {
	t.Helper()
	var m master
	lines := strings.Split(strings.TrimSpace(body), "\n")
	for i := 0; i < len(lines); i++ {
		switch l := lines[i]; {
		case strings.HasPrefix(l, "#EXT-X-MEDIA:"):
			m.media = append(m.media, attrs(t, strings.TrimPrefix(l, "#EXT-X-MEDIA:")))
		case strings.HasPrefix(l, "#EXT-X-STREAM-INF:"):
			a := attrs(t, strings.TrimPrefix(l, "#EXT-X-STREAM-INF:"))
			i++
			a["URI"] = lines[i]
			m.variants = append(m.variants, a)
		case l != "#EXTM3U" && l != "#EXT-X-VERSION:7" && l != "#EXT-X-INDEPENDENT-SEGMENTS":
			t.Fatalf("master line %q", l)
		}
	}
	return m
}

type segment struct {
	uri            string
	offset, length int64
	seconds        float64
}

type mediaPlaylist struct {
	target int
	init   segment
	segs   []segment
}

func parseMedia(t *testing.T, body string) mediaPlaylist {
	t.Helper()
	var p mediaPlaylist
	var cur segment
	lines := strings.Split(body, "\n")
	if lines[len(lines)-2] != "#EXT-X-ENDLIST" {
		t.Fatalf("no ENDLIST: %s", body)
	}
	byteRange := func(s string) (int64, int64) {
		n, o, _ := strings.Cut(s, "@")
		l, err1 := strconv.ParseInt(n, 10, 64)
		off, err2 := strconv.ParseInt(o, 10, 64)
		if err1 != nil || err2 != nil {
			t.Fatalf("byte range %q", s)
		}
		return l, off
	}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "#EXT-X-TARGETDURATION:"):
			p.target, _ = strconv.Atoi(strings.TrimPrefix(l, "#EXT-X-TARGETDURATION:"))
		case strings.HasPrefix(l, "#EXT-X-MAP:"):
			a := attrs(t, strings.TrimPrefix(l, "#EXT-X-MAP:"))
			p.init.uri = a["URI"]
			p.init.length, p.init.offset = byteRange(a["BYTERANGE"])
		case strings.HasPrefix(l, "#EXTINF:"):
			cur.seconds, _ = strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(l, "#EXTINF:"), ","), 64)
		case strings.HasPrefix(l, "#EXT-X-BYTERANGE:"):
			cur.length, cur.offset = byteRange(strings.TrimPrefix(l, "#EXT-X-BYTERANGE:"))
		case l != "" && !strings.HasPrefix(l, "#"):
			cur.uri = l
			if int(math.Round(cur.seconds)) > p.target {
				t.Fatalf("segment of %.3fs exceeds target %d", cur.seconds, p.target)
			}
			p.segs = append(p.segs, cur)
			cur = segment{}
		}
	}
	return p
}

// checkRendition requires playlist to list want as byte ranges of one blob
// URL and every range to return the blob's bytes through the worker.
func (d *delivery) checkRendition(t *testing.T, playlistURL string, blob string, want []media.Segment, mode media.DeliveryMode, full bool) {
	t.Helper()
	p := parseMedia(t, d.playlist(t, playlistURL, media.HLSContentType))
	data := d.blob(t, blob)
	if p.init.offset != 0 || p.init.length != want[0].Offset || len(p.segs) != len(want) {
		t.Fatalf("%s: init %+v, %d segments, want %d", playlistURL, p.init, len(p.segs), len(want))
	}
	u, _ := url.Parse(p.init.uri)
	if !strings.HasSuffix(u.Path, "/blobs/"+blob) {
		t.Fatalf("init URI %s is not blob %s", p.init.uri, blob)
	}
	if tokenized := u.RawQuery != ""; tokenized != (mode == media.DeliverURL || !full) {
		t.Fatalf("%s mode (full %v) URI %s", mode, full, p.init.uri)
	}
	for i, s := range append([]segment{p.init}, p.segs...) {
		if s.uri != p.init.uri {
			t.Fatalf("segment %d URI %s differs from the rendition's blob URL", i, s.uri)
		}
		if i > 0 {
			w := want[i-1]
			if s.offset != w.Offset || s.length != w.Length || math.Abs(s.seconds-w.Seconds) > 0.001 {
				t.Fatalf("segment %d: %+v, want %+v", i, s, w)
			}
		}
		r := get(t, d.client, s.uri, s.offset, s.length)
		if r.status != http.StatusPartialContent || !bytes.Equal(r.body, data[s.offset:s.offset+s.length]) {
			t.Fatalf("segment %d of %s: %d, %d bytes", i, blob, r.status, len(r.body))
		}
	}
	if st := get(t, d.bare, p.init.uri, 0, 16).status; (st == http.StatusPartialContent) != (mode == media.DeliverURL || !full) {
		t.Fatalf("%s without the cookie: %d", p.init.uri, st)
	}
}

func (d *delivery) manifest(t *testing.T) (*media.Manifest, *media.HLS) {
	t.Helper()
	m, _ := d.e.manifest(t)
	return m, m.Files[m.File("source")].HLS
}

func TestPlaybackThroughWorker(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.commit(t, fixture{w: 640, h: 361, secs: 9, audio: 2, subs: true, tone: 440}.make(t), media.OpInsert)
	e.encode(t)

	for _, mode := range []media.DeliveryMode{media.DeliverCookie, media.DeliverURL} {
		t.Run(string(mode), func(t *testing.T) {
			d := newDelivery(t, e, mode)
			m, h := d.manifest(t)
			masterURL := d.url("hls/source/master.m3u8")
			r := get(t, d.client, masterURL)
			if cookie := r.header.Get("Set-Cookie"); (cookie != "") != (mode == media.DeliverCookie) {
				t.Fatalf("%s mode Set-Cookie %q", mode, cookie)
			}
			pl := parseMaster(t, d.playlist(t, masterURL, media.HLSContentType))
			base, _ := url.Parse(masterURL)
			resolve := func(uri string) string { u, _ := url.Parse(uri); return base.ResolveReference(u).String() }

			var audioBW int
			for _, a := range h.Audio {
				audioBW = max(audioBW, a.Bandwidth)
			}
			if len(pl.variants) != len(h.Video) {
				t.Fatalf("%d variants for %d renditions", len(pl.variants), len(h.Video))
			}
			for i, v := range pl.variants {
				j := slices.IndexFunc(h.Video, func(r media.Rendition) bool { return v["URI"] == fmt.Sprintf("video/%d.m3u8", r.Rung) })
				if j < 0 {
					t.Fatalf("variant %d %v: no such rendition", i, v)
				}
				want := h.Video[j]
				if v["BANDWIDTH"] != strconv.Itoa(want.Bandwidth+audioBW) || v["AVERAGE-BANDWIDTH"] != strconv.Itoa(want.Average+audioBW) ||
					v["RESOLUTION"] != fmt.Sprintf("%dx%d", want.Width, want.Height) || v["CODECS"] != want.Codecs+",mp4a.40.2" ||
					v["AUDIO"] != "audio" || v["SUBTITLES"] != "subs" || v["URI"] != fmt.Sprintf("video/%d.m3u8", want.Rung) {
					t.Fatalf("variant %v for %+v", v, want)
				}
				d.checkRendition(t, resolve(v["URI"]), want.Blob, want.Segments, mode, true)
			}
			var audio, subs []map[string]string
			for _, x := range pl.media {
				switch x["TYPE"] {
				case "AUDIO":
					audio = append(audio, x)
				case "SUBTITLES":
					subs = append(subs, x)
				}
			}
			if len(audio) != 2 || audio[0]["LANGUAGE"] != "ja" || audio[0]["DEFAULT"] != "YES" || audio[1]["DEFAULT"] != "NO" ||
				audio[1]["NAME"] != "Commentary" || len(subs) != 1 || subs[0]["LANGUAGE"] != "en" || subs[0]["FORCED"] != "NO" {
				t.Fatalf("renditions %v", pl.media)
			}
			for i, a := range audio {
				d.checkRendition(t, resolve(a["URI"]), h.Audio[i].Blob, h.Audio[i].Segments, mode, true)
			}
			sub := parseMedia(t, d.playlist(t, resolve(subs[0]["URI"]), media.HLSContentType))
			if len(sub.segs) != 1 || math.Abs(sub.segs[0].seconds-9) > 0.5 {
				t.Fatalf("subtitle playlist %+v", sub)
			}
			if r := get(t, d.client, sub.segs[0].uri); r.status != 200 || !bytes.Contains(r.body, []byte("World")) {
				t.Fatalf("subtitles: %d %q", r.status, r.body)
			}
			if only := parseMaster(t, d.playlist(t, masterURL+"?audio=en&subs=", media.HLSContentType)); len(only.media) != 1 ||
				only.media[0]["LANGUAGE"] != "en" || only.media[0]["DEFAULT"] != "YES" || strings.Contains(only.variants[0]["SUBTITLES"], "subs") {
				t.Fatalf("filtered master %+v", only)
			}

			// Sprite: 100 tiles of 0.09 s, row-major.
			vtt := d.playlist(t, d.url("hls/source/sprite.vtt"), media.VTTContentType)
			cues := strings.Split(strings.TrimSpace(vtt), "\n\n")[1:]
			sp := h.Sprite
			if len(cues) != sp.Cols*sp.Rows || !strings.HasPrefix(cues[1], "00:00:00.090 --> 00:00:00.180\n") ||
				!strings.HasSuffix(cues[99], fmt.Sprintf("#xywh=%d,%d,%d,%d", 9*sp.Width, 9*sp.Height, sp.Width, sp.Height)) {
				t.Fatalf("sprite vtt: %d cues, %q … %q", len(cues), cues[1], cues[len(cues)-1])
			}
			img, _, _ := strings.Cut(strings.Split(cues[0], "\n")[1], "#")
			if r := get(t, d.client, img); r.status != 200 || !bytes.Equal(r.body, d.blob(t, sp.Blob)) {
				t.Fatalf("sprite image: %d", r.status)
			}

			// Per-quality download under its display name.
			for _, v := range h.Video {
				key := video.DownloadKey("source", v.Rung)
				r := get(t, d.client, d.url("download/"+key))
				name := fmt.Sprintf("Title (%dp).mp4", v.Rung)
				if r.status != 200 || r.header.Get("Content-Disposition") != token.Attachment(name) ||
					int64(len(r.body)) != m.Downloads[key].Size {
					t.Fatalf("download %s: %d %v", key, r.status, r.header)
				}
			}

			// ffmpeg plays the master playlist over HTTPS with byte ranges.
			if mode == media.DeliverURL {
				out := filepath.Join(t.TempDir(), "played.mp4")
				if b, err := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-i", masterURL,
					"-map", "0:v:0", "-map", "0:a:0", "-c", "copy", "-y", out).CombinedOutput(); err != nil {
					t.Fatalf("ffmpeg: %v: %s", err, b)
				}
				if p := ffprobe(t, out); p.count("video") != 1 || p.count("audio") != 1 || math.Abs(p.duration(t)-9) > 0.5 {
					t.Fatalf("played %+v", p)
				}
			}
		})
	}
}

func TestPlaybackAccess(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.commit(t, fixture{w: 640, h: 361, secs: 5, audio: 1, tone: 440}.make(t), media.OpInsert)
	e.encode(t)
	d := newDelivery(t, e, media.DeliverCookie)
	_, h := d.manifest(t)
	paths := []string{"hls/source/master.m3u8", "hls/source/video/360.m3u8", "hls/source/audio/a1.m3u8",
		"hls/source/sprite.vtt", "download/" + video.DownloadKey("source", 360)}

	for name, res := range map[string]access.Resolution{"hidden": {}, "locked": {Visible: true}} {
		d.verdict.set(res)
		for _, p := range paths {
			if r := get(t, d.client, d.url(p)); r.status != 404 || r.header.Get("Set-Cookie") != "" {
				t.Fatalf("%s viewer got %s: %d %v", name, p, r.status, r.header)
			}
		}
	}

	// A preview cut covering the file plays it on per-file URL tokens (never
	// the folder cookie) and offers no downloads.
	d.verdict.set(access.Resolution{Visible: true, PreviewLimit: 1})
	if r := get(t, d.client, d.url(paths[0])); r.status != 200 || r.header.Get("Set-Cookie") != "" {
		t.Fatalf("preview master: %d %v", r.status, r.header)
	}
	d.checkRendition(t, d.url(paths[1]), h.Video[0].Blob, h.Video[0].Segments, media.DeliverCookie, false)
	if r := get(t, d.client, d.url(paths[4])); r.status != 404 {
		t.Fatalf("preview download: %d", r.status)
	}

	// A viewer mid-stream keeps playing across a source replacement (a new tone,
	// so new audio blobs): the stale ladder is served until the new one is
	// promoted, and its blobs outlive it.
	d.verdict.set(access.Resolution{Visible: true, Accessible: true})
	old := parseMedia(t, d.playlist(t, d.url(paths[2]), media.HLSContentType))
	e.commit(t, fixture{w: 640, h: 361, secs: 5, audio: 1, tone: 550}.make(t), media.OpReplace)
	if stale := parseMedia(t, d.playlist(t, d.url(paths[2]), media.HLSContentType)); stale.init.uri != old.init.uri {
		t.Fatal("stale ladder not served before re-encode")
	}
	e.encode(t)
	fresh := parseMedia(t, d.playlist(t, d.url(paths[2]), media.HLSContentType))
	if fresh.init.uri == old.init.uri {
		t.Fatal("playlist still points at the replaced ladder")
	}
	last := old.segs[len(old.segs)-1]
	if r := get(t, d.client, last.uri, last.offset, last.length); r.status != http.StatusPartialContent ||
		!bytes.Equal(r.body, d.blob(t, h.Audio[0].Blob)[last.offset:last.offset+last.length]) {
		t.Fatalf("old segment after replacement: %d", r.status)
	}
}
