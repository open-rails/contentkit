package video_test

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// loudness is a file's integrated loudness (EBU R128), LUFS.
func loudness(t *testing.T, path string) float64 {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-hide_banner", "-nostdin", "-i", path, "-af", "loudnorm=print_format=json", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("loudness %s: %v: %s", path, err, out)
	}
	var m struct {
		I string `json:"input_i"`
	}
	if err := json.Unmarshal(out[bytes.LastIndexByte(out, '{'):bytes.LastIndexByte(out, '}')+1], &m); err != nil {
		t.Fatal(err)
	}
	v, err := strconv.ParseFloat(m.I, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// An audio upload becomes an HLS track and a faststart M4A at the preset's
// loudness; an unreadable one fails (Hooks.Failed).
func TestAudio(t *testing.T) {
	e := newEnv(t, opts{loudness: -16})
	e.start()
	tone := filepath.Join(t.TempDir(), "tone.wav")
	if b, err := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-f", "lavfi", "-i", "sine=frequency=440:duration=6,volume=0.05",
		"-ar", "48000", "-c:a", "pcm_s16le", "-y", tone).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, b)
	}
	noise := filepath.Join(t.TempDir(), "noise.wav")
	if err := os.WriteFile(noise, bytes.Repeat([]byte("not audio "), 100), 0o600); err != nil {
		t.Fatal(err)
	}
	e.put("audio/tone.wav", "audio/wav", tone, nil)
	e.put("audio/noise.wav", "audio/wav", noise, nil)
	e.wait()

	m := e.manifest()
	src := e.file(m, "audio/tone.wav")
	if math.Abs(src.Dur-6) > 0.1 || len(src.Pending) != 0 {
		t.Fatalf("upload %+v", src)
	}
	if got := outputPaths(m, "audio/tone.wav", "listen"); !slices.Equal(got, []string{"listen/tone/audio.mp4", "listen/tone/audio.m4a"}) {
		t.Fatalf("outputs %v", got)
	}
	hls, m4a := e.file(m, "listen/tone/audio.mp4"), e.file(m, "listen/tone/audio.m4a")
	if tr := hls.Track; tr == nil || tr.Kind != media.TrackAudio || tr.ID != "a1" || !tr.Default || tr.Codecs != "mp4a.40.2" || tr.Bandwidth <= 0 ||
		hls.FP == "" || hls.FP != m4a.FP || m4a.Type != "audio/mp4" || math.Abs(m4a.Dur-6) > 0.1 {
		t.Fatalf("outputs %+v %+v", hls, m4a)
	}
	checkByteRanges(t, e.blob(hls.Blob), e.index(hls).Segments, "audio", 6)
	path := e.blob(m4a.Blob)
	body := must(os.ReadFile(path))
	if l := loudness(t, path); math.Abs(l+16) > 1.5 || bytes.Index(body, []byte("moov")) > bytes.Index(body, []byte("mdat")) {
		t.Fatalf("m4a at %.1f LUFS, faststart %v", l, bytes.Index(body, []byte("moov")) < bytes.Index(body, []byte("mdat")))
	}
	bad := e.file(m, "audio/noise.wav")
	if bad.Fail() == nil || len(bad.Pending) != 0 || len(m.Outputs("audio/noise.wav", "listen")) != 0 ||
		len(e.failed()) != 1 || e.failed()[0].path != "audio/noise.wav" {
		t.Fatalf("unreadable audio %+v, failures %+v", bad, e.failed())
	}
}

// Subtitle sidecars become clean UTF-8 WebVTT: SRT decoded by its language's
// charsets or its declared charset; one that cannot be read fails. A
// charset or language change converts again.
func TestSubtitleSidecars(t *testing.T) {
	e := newEnv(t, opts{})
	e.start()
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	srt := func(text string) string {
		return "1\r\n00:00:01,000 --> 00:00:02,500\r\n<font color=red>" + text + "</font>\r\n\r\n2\r\n0:0:3.5 --> 0:0:4\r\n<i>" + text + "!</i>\r\n"
	}
	sjis := must(japanese.ShiftJIS.NewEncoder().Bytes([]byte(srt("こんにちは"))))
	euckr := must(korean.EUCKR.NewEncoder().Bytes([]byte(srt("안녕하세요"))))
	e.put("subs/en", "application/x-subrip", write("en.srt", []byte(srt("Hello & bye"))), map[string]any{"lang": "EN"})
	ja := write("ja.srt", sjis)
	e.put("subs/ja", "application/x-subrip", ja, map[string]any{"lang": "ja"})
	e.put("subs/ko", "application/x-subrip", write("ko.srt", euckr), map[string]any{"charset": "EUC-KR"})
	e.put("subs/de", "text/vtt", write("de.vtt", []byte("WEBVTT\n\nSTYLE\n::cue { color: red }\n\n00:00:01.000 --> 00:00:02.000 align:start\n<v Anna>Hallo</v> Welt\n")), nil)
	e.put("subs/bad", "application/x-subrip", write("bad.srt", []byte("just some words\n")), nil)
	e.wait()

	m := e.manifest()
	for path, want := range map[string]string{"subs/en.srt": "\nHello &amp; bye\n", "subs/ja.srt": "<i>こんにちは!</i>", "subs/ko.srt": "<i>안녕하세요!</i>",
		"subs/de.vtt": "00:00:01.000 --> 00:00:02.000 align:start\nHallo Welt\n"} {
		out := m.Outputs(path, "vtt")
		stem, _, _ := strings.Cut(strings.TrimPrefix(path, "subs/"), ".")
		if len(out) != 1 || out[0].Path != "vtt/"+stem+".vtt" || out[0].Type != "text/vtt" || out[0].FP == "" || out[0].Track != nil {
			t.Fatalf("%s outputs %+v", path, out)
		}
		vtt := string(must(os.ReadFile(e.blob(out[0].Blob))))
		if !strings.HasPrefix(vtt, "WEBVTT\n") || !strings.Contains(vtt, want) || strings.Contains(vtt, "font") || strings.Contains(vtt, "STYLE") {
			t.Fatalf("%s converted to %q", path, vtt)
		}
		if strings.HasSuffix(path, ".srt") && !strings.Contains(vtt, "00:00:03.500 --> 00:00:04.000") {
			t.Fatalf("%s timings %q", path, vtt)
		}
	}
	bad := e.file(m, "subs/bad.srt")
	if bad.Fail() == nil || len(m.Outputs("subs/bad.srt", "vtt")) != 0 || !slices.ContainsFunc(e.failed(), func(f failure) bool { return f.path == "subs/bad.srt" }) {
		t.Fatalf("unreadable subtitles %+v", bad)
	}

	// The same bytes with a declared charset: a new fingerprint, the same text.
	before := m.Outputs("subs/ja.srt", "vtt")[0]
	e.put("subs/ja", "application/x-subrip", ja, map[string]any{"lang": "ja", "charset": "Shift_JIS"})
	e.wait()
	after := e.manifest().Outputs("subs/ja.srt", "vtt")[0]
	oldSum, oldValid := layout.BlobDigest(before.Blob)
	newSum, newValid := layout.BlobDigest(after.Blob)
	if after.FP == before.FP || after.Blob == before.Blob || !oldValid || !newValid || !bytes.Equal(oldSum, newSum) {
		t.Fatalf("re-converted %+v, before %+v", after, before)
	}
}

// The commit's projection covers what audio uploads gain once processed,
// whatever their titles (the final review: a title of 120 quotes took 899
// bytes against 708 projected), so an audio item admitted at the bound does
// not go Full.
func TestProjectionCoversAudio(t *testing.T) {
	e := newEnv(t, opts{})
	dir := t.TempDir()
	titles := map[string]string{"plain": "", "ascii": strings.Repeat("T", 200), "quotes": strings.Repeat(`"`, 200), "slashes": strings.Repeat(`\`, 200)}
	for name, title := range titles {
		wav := filepath.Join(dir, name+".wav")
		if b, err := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-f", "lavfi", "-i", "sine=frequency=440:duration=1",
			"-c:a", "pcm_s16le", "-metadata", "title="+title, "-y", wav).CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v: %s", err, b)
		}
		e.put("audio/"+name+".wav", "audio/wav", wav, nil)
	}
	size := func(m *media.Manifest) int64 {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(m); err != nil {
			t.Fatal(err)
		}
		return int64(b.Len())
	}
	before := e.manifest()
	projected := e.item().Kind().Unwritten(before)
	e.start()
	e.wait()
	after := e.manifest()
	if after.Full || size(after)-size(before) > projected {
		t.Fatalf("full %v: processing added %d bytes, %d projected", after.Full, size(after)-size(before), projected)
	}
	for name, title := range titles {
		hls := e.file(after, "listen/"+name+"/audio.mp4")
		if hls.Track == nil || title != "" && !strings.HasPrefix(title, hls.Track.Label) || title != "" && hls.Track.Label == "" {
			t.Fatalf("%s: track %+v", name, hls.Track)
		}
		if b, _ := json.Marshal(hls.Track.Label); len(b)-2 > 120 {
			t.Fatalf("%s: label takes %d bytes of JSON", name, len(b)-2)
		}
	}
}
