package video_test

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"

	"github.com/open-rails/contentkit/media/layout"
)

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
