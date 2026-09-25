package video_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"golang.org/x/text/encoding/japanese"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/video"
)

// putSidecar uploads body as an original and returns the op inserting it
// (or replacing it) as file name with meta.
func (e *env) putSidecar(t *testing.T, name, op string, body []byte, meta map[string]any) media.Op {
	t.Helper()
	sum := sha256.Sum256(body)
	orig := media.SHA256Name(sum[:])
	key, _ := e.item(t).Original(orig)
	if _, err := e.store.Put(context.Background(), key, bytes.NewReader(body), int64(len(body)),
		media.PutOptions{ContentType: media.SubtitleType(name), ChecksumSHA256: sum[:]}); err != nil {
		t.Fatal(err)
	}
	return media.Op{Op: op, Name: name, Original: orig, Meta: meta}
}

func (e *env) commitOps(t *testing.T, ops ...media.Op) {
	t.Helper()
	if _, err := e.uploads.Commit(context.Background(), admin, e.ref, ops); err != nil {
		t.Fatal(err)
	}
}

const styledASS = `[Script Info]
ScriptType: v4.00+

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,48,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,0,2,10,10,10,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:00.50,0:00:02.00,Default,,0,0,0,,{\an8\pos(960,50)\c&H0000FF&}Sign {\b1}bold{\b0} <script>x</script> & more\Nline two
Comment: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,a comment
Dialogue: 0,0:00:03.00,0:00:05.00,Default,,0,0,0,,{\p1}m 0 0 l 100 0 100 100 0 100{\p0}
`

func TestSubtitleSidecars(t *testing.T) {
	e, failed := newAudioEnv(t, media.Audio{}, nil)
	ctx := context.Background()
	encode := func() {
		t.Helper()
		if err := e.encoder.Encode(ctx, video.Job{Ref: e.ref, Versioned: true, Video: media.Video{PosterWidths: posterWidths}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	e.commit(t, fixture{w: 640, h: 361, secs: 6, audio: 1, subs: true, tone: 440}.make(t), media.OpInsert)
	encode()
	before, _ := e.manifest(t)
	ladder := before.Files[0].HLS
	blobs := e.blobs(t)

	sjis, err := japanese.ShiftJIS.NewEncoder().Bytes([]byte("1\r\n00:00:01,000 --> 00:00:03,000\r\nお前はもう死んでいる。<i>何？</i>\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	vtt := "WEBVTT\n\nSTYLE\n::cue { color: red }\n\n00:00:01.000 --> 00:00:02.000 align:start\n<v Anna>Hallo</v> Welt\n"
	e.commitOps(t,
		e.putSidecar(t, "ja.srt", media.OpInsert, sjis, nil),
		e.putSidecar(t, "signs.ass", media.OpInsert, []byte(styledASS), map[string]any{media.MetaLang: "eng", media.MetaForced: true, media.MetaLabel: "Signs"}),
		e.putSidecar(t, "de.vtt", media.OpInsert, []byte(vtt), map[string]any{media.MetaLang: "de"}),
		e.putSidecar(t, "bad.srt", media.OpInsert, []byte("just some words\n"), nil),
		e.putSidecar(t, "other.srt", media.OpInsert, []byte("1\n00:00:01,000 --> 00:00:02,000\nx\n"), map[string]any{media.MetaFor: "nope"}),
	)
	encode()

	m, _ := e.manifest(t)
	if !reflect.DeepEqual(m.Files[0].HLS, ladder) {
		t.Fatal("adding subtitles changed the video's ladder")
	}
	var added []string
	for _, b := range e.blobs(t) {
		if !slices.Contains(blobs, b) {
			added = append(added, b)
		}
	}
	if len(added) != 4 { // four converted sidecars, no renditions
		t.Fatalf("new blobs %v", added)
	}
	bad := m.Files[m.File("bad.srt")]
	if bad.State() != media.StateFailed || bad.Failed() == nil || !slices.Equal(*failed, []string{"bad.srt"}) {
		t.Fatalf("bad.srt: %+v, failed %v", bad, *failed)
	}
	if r, err := e.manifests.Readiness(ctx, e.ref); err != nil || !slices.Equal(r.Failed, []string{"v1/bad.srt"}) || !slices.Equal(r.Processing, []string{media.PosterSlot}) {
		t.Fatalf("readiness %+v %v", r, err)
	}
	ja := m.Files[m.File("ja.srt")]
	if ja.State() != media.StateReady || ja.Meta[media.MetaLang] != nil || ja.Meta[media.MetaLabel] != "Subtitles" {
		t.Fatalf("ja.srt: %+v", ja)
	}
	if m.Files[m.File("signs.ass")].Meta[media.MetaLang] != "en" {
		t.Fatal("language not normalized")
	}

	d := newDelivery(t, e, media.DeliverURL)
	masterURL := d.url("hls/source/master.m3u8")
	base, _ := url.Parse(masterURL)
	subs := func() map[string]map[string]string {
		out := map[string]map[string]string{}
		for _, x := range parseMaster(t, d.playlist(t, masterURL, media.HLSContentType)).media {
			if x["TYPE"] == "SUBTITLES" {
				out[x["URI"]] = x
			}
		}
		return out
	}
	track := func(uri string) string {
		t.Helper()
		u, _ := url.Parse(uri)
		p := parseMedia(t, d.playlist(t, base.ResolveReference(u).String(), media.HLSContentType))
		r := get(t, d.client, p.segs[0].uri)
		if r.status != 200 {
			t.Fatalf("%s: %d", uri, r.status)
		}
		return string(r.body)
	}
	got := subs()
	if len(got) != 4 || got["subs/s1.m3u8"]["LANGUAGE"] != "en" || got["subs/ja.srt.m3u8"]["NAME"] != "Subtitles" ||
		got["subs/signs.ass.m3u8"]["FORCED"] != "YES" || got["subs/signs.ass.m3u8"]["NAME"] != "Signs" ||
		got["subs/de.vtt.m3u8"]["LANGUAGE"] != "de" || got["subs/de.vtt.m3u8"]["NAME"] != "German" {
		t.Fatalf("subtitle renditions %v", got)
	}
	for uri, want := range map[string][]string{
		"subs/s1.m3u8":        {"Hello", "World"},
		"subs/ja.srt.m3u8":    {"お前はもう死んでいる。<i>何？</i>"},
		"subs/signs.ass.m3u8": {"Sign <b>bold</b> x &amp; more\nline two"},
		"subs/de.vtt.m3u8":    {"00:00:01.000 --> 00:00:02.000 align:start\nHallo Welt\n"},
	} {
		body := track(uri)
		for _, w := range want {
			if !strings.HasPrefix(body, "WEBVTT\n") || !strings.Contains(body, w) {
				t.Fatalf("%s:\n%s\nwant %q", uri, body, w)
			}
		}
		if strings.Contains(body, "script") || strings.Contains(body, "m 0 0") || strings.Contains(body, "STYLE") || strings.Contains(body, `{\`) {
			t.Fatalf("%s not cleaned:\n%s", uri, body)
		}
	}
	read, _, err := d.e.manifests.Get(ctx, e.ref)
	if err != nil || read.Files[read.File("de.vtt")].Variants[media.SubtitleVariant].Type != "text/vtt" {
		t.Fatalf("vtt variant: %v", err)
	}

	// A preview cut covering only the video lists only its own tracks.
	d.verdict.set(access.Resolution{Visible: true, PreviewLimit: 1})
	if got := subs(); len(got) != 1 || got["subs/s1.m3u8"] == nil {
		t.Fatalf("preview subtitles %v", got)
	}
	d.verdict.set(access.Resolution{Visible: true, Accessible: true})

	// Replace and remove republish the playlists without re-encoding.
	e.commitOps(t, media.Op{Op: media.OpRemove, Name: "de.vtt"},
		e.putSidecar(t, "ja.srt", media.OpReplace, []byte("1\n00:00:01,000 --> 00:00:02,000\nこんにちは\n"), map[string]any{media.MetaLang: "ja"}))
	encode()
	m, _ = e.manifest(t)
	if !reflect.DeepEqual(m.Files[0].HLS, ladder) {
		t.Fatal("replacing subtitles changed the video's ladder")
	}
	got = subs()
	if len(got) != 3 || got["subs/de.vtt.m3u8"] != nil || got["subs/ja.srt.m3u8"]["LANGUAGE"] != "ja" || got["subs/ja.srt.m3u8"]["NAME"] != "Japanese" {
		t.Fatalf("after replace/remove %v", got)
	}
	if body := track("subs/ja.srt.m3u8"); !strings.Contains(body, "こんにちは") {
		t.Fatalf("replaced track:\n%s", body)
	}
}

// Converting is idempotent under a normalized language, SRT timings are
// lenient, sidecars follow a rename, source tracks re-extract on a new
// SubsSpec without re-encoding, and both kinds of subtitle are size-capped.
func TestSubtitleRecipes(t *testing.T) {
	e, _ := newAudioEnv(t, media.Audio{}, nil)
	ctx := context.Background()
	encode := func() {
		t.Helper()
		if err := e.encoder.Encode(ctx, video.Job{Ref: e.ref, Versioned: true, Video: media.Video{PosterWidths: posterWidths}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	e.commit(t, fixture{w: 640, h: 361, secs: 6, audio: 1, subs: true, tone: 440}.make(t), media.OpInsert)
	e.commitOps(t, e.putSidecar(t, "short.srt", media.OpInsert, []byte("1\n00:01,5 --> 00:02,25\nlenient\n"),
		map[string]any{media.MetaLang: "eng", media.MetaFor: "source"}))
	encode()
	m, etag := e.manifest(t)
	sc := m.Files[m.File("short.srt")]
	if sc.State() != media.StateReady || sc.Meta[media.MetaLang] != "en" {
		t.Fatalf("short.srt: %+v", sc)
	}
	vtt, err := os.ReadFile(e.blob(t, sc.Variants[media.SubtitleVariant].Blob))
	if err != nil || !strings.Contains(string(vtt), "00:00:01.500 --> 00:00:02.250\nlenient") {
		t.Fatalf("lenient SRT timings:\n%s %v", vtt, err)
	}
	if h := m.Files[0].HLS; h.SubsSpec != video.SubsSpec || len(h.Subs) != 1 {
		t.Fatalf("source tracks %+v", h)
	}
	blobs := e.blobs(t)
	encode()
	if _, again := e.manifest(t); again != etag || !slices.Equal(e.blobs(t), blobs) {
		t.Fatal("a converted sidecar was converted again")
	}

	e.commitOps(t, media.Op{Op: media.OpRename, Name: "source", To: "movie"})
	if m, _ := e.manifest(t); m.Files[m.File("short.srt")].Meta[media.MetaFor] != "movie" {
		t.Fatal("meta.for did not follow the rename")
	}

	// An older SubsSpec: the source's tracks are extracted again, the
	// renditions untouched; over the cap, a track is dropped.
	stale := func() *media.HLS {
		t.Helper()
		var ladder *media.HLS
		if _, err := e.manifests.Edit(ctx, e.ref, func(m *media.Manifest) error {
			h := *m.Files[0].HLS
			h.SubsSpec, h.Subs = "old", nil
			m.Files[0].HLS, ladder = &h, &h
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return ladder
	}
	before := stale()
	encode()
	m, _ = e.manifest(t)
	if h := m.Files[0].HLS; h.SubsSpec != video.SubsSpec || len(h.Subs) != 1 || !reflect.DeepEqual(h.Video, before.Video) {
		t.Fatalf("re-extracted: %+v", h)
	}
	defer video.SetMaxSubtitleBytes(40)()
	stale()
	encode()
	if m, _ = e.manifest(t); len(m.Files[0].HLS.Subs) != 0 || m.Files[0].HLS.SubsSpec != video.SubsSpec {
		t.Fatalf("an oversized source track was kept: %+v", m.Files[0].HLS)
	}

	// An oversized sidecar fails before it is downloaded.
	e.commitOps(t, e.putSidecar(t, "big.srt", media.OpInsert, []byte("1\n00:00:01,000 --> 00:00:02,000\n"+strings.Repeat("x", 100)+"\n"), nil))
	encode()
	if m, _ = e.manifest(t); m.Files[m.File("big.srt")].Failed() == nil || !strings.Contains(m.Files[m.File("big.srt")].Failed().Message, "at most 40") {
		t.Fatalf("big.srt: %+v", m.Files[m.File("big.srt")])
	}
}
