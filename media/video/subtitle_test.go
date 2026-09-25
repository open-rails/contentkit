package video

import (
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	xunicode "golang.org/x/text/encoding/unicode"
)

func TestCleanVTT(t *testing.T) {
	in := "\uFEFFWEBVTT - title\r\n\r\nSTYLE\r\n::cue { color: red }\r\n\r\nNOTE a note\r\n\r\nREGION\r\nid:r1\r\n\r\n" +
		"intro\r\n00:00:03.000 --> 00:00:04.000 line:0 align:start region:r1 bogus\r\n<v Bob>Hi</v> <c.red>there</c> &amp; <script>x</script> 1 < 2 --> 3\r\n\r\n" +
		"00:00.500 --> 00:02.000\r\n<b>bold <i>both</b> tail\r\n\r\n" +
		"00:00:05.000 --> 00:00:05.000\r\nzero length\r\n\r\n" +
		"00:00:06.000 --> 00:00:07.000\r\nm 0 0 l 100 0 100 100 0 100\r\n\r\n" +
		"00:00:08.000 --> 00:00:09.000\r\n<b></b>\r\n\r\n" +
		"99:00:00.000 --> 99:00:01.000\r\ni\r\n"
	out, n, err := cleanVTT([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	want := "WEBVTT\n" +
		"\n00:00:00.500 --> 00:00:02.000\n<b>bold <i>both</i></b> tail\n" +
		"\n00:00:03.000 --> 00:00:04.000 line:0 align:start\nHi there &amp; x 1 &lt; 2 --&gt; 3\n" +
		"\n99:00:00.000 --> 99:00:01.000\ni\n"
	if string(out) != want || n != 3 {
		t.Fatalf("got %d cues:\n%s\nwant:\n%s", n, out, want)
	}
	if _, _, err := cleanVTT([]byte("1\n00:00:01,000 --> 00:00:02,000\nsrt\n")); err == nil {
		t.Fatal("SRT accepted as WebVTT")
	}
}

func TestDecodeText(t *testing.T) {
	encode := func(name string, s string) []byte {
		t.Helper()
		var b []byte
		var err error
		switch name {
		case "sjis":
			b, err = japanese.ShiftJIS.NewEncoder().Bytes([]byte(s))
		case "eucjp":
			b, err = japanese.EUCJP.NewEncoder().Bytes([]byte(s))
		case "gbk":
			b, err = simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
		case "big5":
			b, err = traditionalchinese.Big5.NewEncoder().Bytes([]byte(s))
		case "euckr":
			b, err = korean.EUCKR.NewEncoder().Bytes([]byte(s))
		case "1251":
			b, err = charmap.Windows1251.NewEncoder().Bytes([]byte(s))
		case "1252":
			b, err = charmap.Windows1252.NewEncoder().Bytes([]byte(s))
		case "utf16le":
			b, err = xunicode.UTF16(xunicode.LittleEndian, xunicode.UseBOM).NewEncoder().Bytes([]byte(s))
		case "utf16le-nobom":
			b, err = xunicode.UTF16(xunicode.LittleEndian, xunicode.IgnoreBOM).NewEncoder().Bytes([]byte(s))
		case "utf16be":
			b, err = xunicode.UTF16(xunicode.BigEndian, xunicode.UseBOM).NewEncoder().Bytes([]byte(s))
		case "utf8bom":
			b = append([]byte{0xEF, 0xBB, 0xBF}, s...)
		case "utf8":
			b = []byte(s)
		}
		if err != nil {
			t.Fatalf("encode %s: %v", name, err)
		}
		return b
	}
	srt := func(text string) string { return "1\n00:00:01,000 --> 00:00:02,000\n" + text + "\n" }
	ja := srt("お前はもう死んでいる。何だと？ 東京へ行きましょう。")
	zhs := srt("你好，世界。我们今天去北京吃饭吧。这是一个测试字幕文件。")
	zht := srt("你好，世界。我們今天去台北吃飯吧。這是一個測試字幕檔案。")
	ko := srt("안녕하세요 세계. 오늘 서울에 가서 밥을 먹읍시다.")
	ru := srt("Привет, мир. Давайте сегодня пойдём в Москву.")
	fr := srt("Déjà vu : l'été à Noël, où ça ? Très bien, garçon.")
	de := srt("Für Größe: Ärger über Übermaß. Straße, schön!")
	de2 := srt("Größe")
	for _, c := range []struct {
		enc, text, lang, charset string
	}{
		{"utf8", ja, "", ""}, {"utf8bom", ja, "", ""}, {"utf16le", ja, "", ""}, {"utf16be", ru, "", ""}, {"utf16le-nobom", fr, "", ""},
		{"sjis", ja, "", ""}, {"sjis", ja, "ja", ""}, {"eucjp", ja, "ja", ""}, {"eucjp", ja, "", ""},
		{"gbk", zhs, "", ""}, {"gbk", zhs, "zh-Hans", ""}, {"big5", zht, "zh-TW", ""},
		{"euckr", ko, "", ""}, {"1251", ru, "", ""}, {"1251", ru, "ru", ""}, {"1252", fr, "", ""},
		{"1252", de, "", ""}, {"1252", de2, "", ""}, {"1252", de, "de", ""}, // accented Latin is not GB18030 or Big5
		{"1252", fr, "ja", ""}, // a wrong hint falls through to detection
		{"big5", zht, "", "big5"},
	} {
		got, err := decodeText(encode(c.enc, c.text), c.lang, c.charset)
		if err != nil || got != c.text {
			t.Errorf("%s lang %q charset %q: %q, %v", c.enc, c.lang, c.charset, got, err)
		}
	}
	if _, err := decodeText([]byte("x"), "", "no-such-charset"); err == nil || !strings.Contains(err.Error(), "charset") {
		t.Fatalf("unknown charset: %v", err)
	}
}

func TestSubtitleSourcePrep(t *testing.T) {
	srt := "1\r\n01:02,5 --> 01:04,25\r\nshort\r\n\r\n2\r\n1:2:3.4 --> 01:02:04.456 X1:1\r\nlong\r\n"
	want := "1\r\n00:01:02,500 --> 00:01:04,250\r\nshort\r\n\r\n2\r\n01:02:03,400 --> 01:02:04,456 X1:1\r\nlong\r\n"
	if got := normalizeSRT(srt); got != want {
		t.Fatalf("normalizeSRT:\n%q\nwant\n%q", got, want)
	}
	ass := "[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n" +
		"Dialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,{\\an8}Keep {TL note: pun} this, {\\p1}m 0 0 l 10 0{\\p0} too\n" +
		"Dialogue: 0,0:00:03.00,0:00:04.00,Default,,0,0,0,,{\\an7\\p2}m 0 0 l 5 5\n"
	got := stripASS(ass)
	if !strings.Contains(got, `,,{\an8}Keep  this,  too`) || strings.Contains(got, "TL note") || strings.Contains(got, "m 0 0") {
		t.Fatalf("stripASS:\n%s", got)
	}
}
