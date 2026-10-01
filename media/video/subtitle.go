package video

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	textenc "golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	xunicode "golang.org/x/text/encoding/unicode"
	"golang.org/x/text/encoding/unicode/utf32"
	"golang.org/x/text/language"

	"github.com/open-rails/contentkit/media"
)

// cleanRecipe versions cleanVTT and the conversions before it: a change
// converts sidecars and an HLS preset's source text tracks again (subsFP),
// never the ladder.
const cleanRecipe = "webvtt|utf8|b-i-u-only|no-ass-drawings|srt-lenient-times|max-32m|v2"

// sidecarOf is a subtitle upload's conversion inputs from its meta: the
// charset that overrides detection and the normalized language that hints it.
func sidecarOf(f media.File) (charset, lang string) {
	charset, _ = f.Meta[media.MetaCharset].(string)
	lang, _ = f.Meta[media.MetaLang].(string)
	lang, _ = normalizeLanguage(lang)
	return strings.ToLower(charset), lang
}

// subtitleFP covers the sidecar's charset and normalized language.
func subtitleFP(f media.File, s *media.Subtitles) string {
	charset, lang := sidecarOf(f)
	return media.SpecFP(f, s, cleanRecipe+"|charset:"+charset+"|lang:"+lang)
}

// maxSubtitleBytes bounds a subtitle read into memory: a sidecar, or a
// source's text track as converted.
var maxSubtitleBytes int64 = 32 << 20

// subtitle converts a sidecar to clean UTF-8 WebVTT: SRT and SSA/ASS
// through ffmpeg's WebVTT encoder (as the ladder converts a source's text
// tracks), then cleanVTT; WebVTT through cleanVTT alone. The reader lists
// it as a track from the upload's meta.
func (e *Encoder) subtitle(ctx context.Context, item media.Item, f media.File, p *media.Private) error {
	if to := item.Kind().OutputPath(p, f.Path); to == f.Path {
		return &PermanentError{fmt.Errorf("preset %s would write over its own upload %s", p.Name, to)}
	}
	key, _ := item.Blob(f.Blob)
	if obj, err := e.store.Head(ctx, key); errors.Is(err, media.ErrNotFound) {
		return errStale
	} else if err != nil {
		return err
	} else if obj.Size > maxSubtitleBytes {
		return &PermanentError{fmt.Errorf("subtitles are %d bytes; at most %d", obj.Size, maxSubtitleBytes)}
	}
	dir, err := os.MkdirTemp(e.c.TempDir, tempPattern)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "source")
	if err := e.fetch(ctx, item, f.Blob, src, nil); err != nil {
		return err
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	charset, _ := sidecarOf(f)
	lang, _ := f.Meta[media.MetaLang].(string)
	vtt, err := e.convertSubtitle(ctx, dir, f.Type, raw, lang, charset)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "out.vtt")
	if err := os.WriteFile(path, vtt, 0o600); err != nil {
		return err
	}
	blob, size, err := e.put(ctx, item, path, "text/vtt", nil)
	if err != nil {
		return err
	}
	fp := subtitleFP(f, p.Subtitles)
	out := media.File{Path: item.Kind().OutputPath(p, f.Path), Blob: blob, Type: "text/vtt", Size: size, FP: fp}
	return e.publish(ctx, item, []media.File{out}, func(m *media.Manifest) error {
		g, err := current(m, f.Path, f.Blob)
		if err != nil || subtitleFP(g, p.Subtitles) != fp {
			return errStale
		}
		return m.SetOutputs(g.Path, p.Name, []media.File{out})
	})
}

// subtitleDemuxers are the sidecar formats ffmpeg reads (SSA through "ass").
var subtitleDemuxers = []string{"srt", "ass"}

// convertSubtitle converts a sidecar of type typ (its parser: WebVTT, SRT or
// SSA/ASS, never sniffed) to clean WebVTT.
func (e *Encoder) convertSubtitle(ctx context.Context, dir, typ string, raw []byte, lang, charset string) ([]byte, error) {
	text, err := decodeText(raw, lang, charset)
	if err != nil {
		return nil, &PermanentError{err}
	}
	var vtt []byte
	switch typ {
	case "text/vtt":
		vtt = []byte(text)
	case "application/x-subrip", "text/x-ssa", "text/x-ass":
		in, format := filepath.Join(dir, "in.srt"), "srt"
		if typ == "application/x-subrip" {
			text = normalizeSRT(text)
		} else {
			in, format = filepath.Join(dir, "in.ass"), "ass"
			text = stripASS(text)
		}
		if err := os.WriteFile(in, []byte(text), 0o600); err != nil {
			return nil, err
		}
		out := filepath.Join(dir, "ffmpeg.vtt")
		args := append([]string{"-v", "error", "-nostdin"}, inputOptions([]string{format})...)
		args = append(args, "-i", in, "-map", "0:s:0")
		if _, err := command(ctx, "ffmpeg", append(args, webvttArgs(out)...)...); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &PermanentError{fmt.Errorf("unreadable subtitles: %w", err)}
		}
		if vtt, err = os.ReadFile(out); err != nil {
			return nil, err
		}
	default:
		return nil, &PermanentError{fmt.Errorf("%s is not a subtitle type", typ)}
	}
	clean, cues, err := cleanVTT(vtt)
	if err != nil {
		return nil, &PermanentError{err}
	}
	if cues == 0 {
		return nil, &PermanentError{errors.New("subtitles have no cues")}
	}
	return clean, nil
}

var srtTiming = regexp.MustCompile(`(?m)^[ \t]*` + srtTime + `[ \t]*-->[ \t]*` + srtTime + `(.*)$`)

const srtTime = `(?:(\d{1,3}):)?(\d{1,2}):(\d{1,2})(?:[,.](\d{1,3}))?`

// normalizeSRT rewrites timings lenient players accept ("01:02,5",
// "1:2:3.45") to SRT's HH:MM:SS,mmm, a short fraction being tenths or
// hundredths.
func normalizeSRT(s string) string {
	stamp := func(h, m, sec, frac string) string {
		n := func(v string) int { i, _ := strconv.Atoi(v); return i }
		ms := n((frac + "000")[:3])
		return fmt.Sprintf("%02d:%02d:%02d,%03d", n(h), n(m), n(sec), ms)
	}
	return srtTiming.ReplaceAllStringFunc(s, func(l string) string {
		m := srtTiming.FindStringSubmatch(l)
		return stamp(m[1], m[2], m[3], m[4]) + " --> " + stamp(m[5], m[6], m[7], m[8]) + m[9]
	})
}

var (
	assDrawingTags = regexp.MustCompile(`\{[^}]*\\p[1-9][^}]*\}.*?(?:\{[^}]*\\p0[^}]*\}|$)`)
	assComment     = regexp.MustCompile(`\{[^\\}][^}]*\}`)
)

// stripASS drops vector drawings ({\p1}…{\p0}) and plain {comment} blocks
// from dialogue text before ffmpeg, which would render both as text.
func stripASS(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if !strings.HasPrefix(strings.TrimSpace(l), "Dialogue:") {
			continue
		}
		// The text is the tenth field; earlier ones hold no braces.
		fields := strings.SplitN(l, ",", 10)
		if len(fields) < 10 {
			continue
		}
		t := assDrawingTags.ReplaceAllString(fields[9], "")
		fields[9] = assComment.ReplaceAllString(t, "")
		lines[i] = strings.Join(fields, ",")
	}
	return strings.Join(lines, "\n")
}

// webvttArgs are the WebVTT output of the mapped text subtitle stream, for
// a source's tracks in the ladder and for sidecars; cleanVTT follows.
func webvttArgs(out string) []string {
	return []string{"-c:s", "webvtt", "-f", "webvtt", "-y", out}
}

// keepSubs cleans a pass's converted text tracks (dir/s{i}.vtt) and drops,
// renumbering the rest, those over maxSubtitleBytes (never read into
// memory), unreadable or without cues.
func keepSubs(ctx context.Context, log *slog.Logger, dir string, subs []track) ([]track, error) {
	var kept []track
	for i, s := range subs {
		path := filepath.Join(dir, fmt.Sprintf("s%d.vtt", i))
		st, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		var clean []byte
		cues := 0
		if st.Size() <= maxSubtitleBytes {
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			clean, cues, _ = cleanVTT(b)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
		if cues == 0 {
			log.WarnContext(ctx, "media/video: subtitle track dropped", "track", s.id, "bytes", st.Size())
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("s%d.vtt", len(kept))), clean, 0o600); err != nil {
			return nil, err
		}
		kept = append(kept, s)
	}
	return kept, nil
}

var (
	vttTiming  = regexp.MustCompile(`^((?:\d+:)?\d{2}:\d{2}\.\d{3})[ \t]+-->[ \t]+((?:\d+:)?\d{2}:\d{2}\.\d{3})(?:[ \t]+(.*))?$`)
	vttSetting = regexp.MustCompile(`^(?:vertical:(?:rl|lr)|line:-?\d+(?:\.\d+)?%?(?:,(?:start|center|end))?|` +
		`position:\d+(?:\.\d+)?%(?:,(?:line-left|center|line-right))?|size:\d+(?:\.\d+)?%|align:(?:start|center|end|left|right))$`)
	// An ASS vector drawing ({\p1}m 0 0 l 100 0 …) left as text by ffmpeg.
	markup     = regexp.MustCompile(`</?[biu]>`)
	vttTag     = regexp.MustCompile(`^(?:/?[a-z][^<]*|\d+:[\d:.]+)$`)
	assDrawing = regexp.MustCompile(`^m\s+-?\d+(?:\.\d+)?\s+-?\d+(?:\.\d+)?(?:\s+(?:[mnlbspc]|-?\d+(?:\.\d+)?))*$`)
)

type vttCue struct {
	start, end int64 // ms
	settings   string
	text       string
}

// cleanVTT rewrites WebVTT to what any player may render as text: the
// header, then valid cues in start order with only b/i/u markup (other tags
// dropped, their text kept), text escaped, safe cue settings kept, and no
// NOTE, STYLE or REGION blocks. Cues left empty, or holding an ASS drawing,
// are dropped. It returns the cue count.
func cleanVTT(in []byte) ([]byte, int, error) {
	s := strings.TrimPrefix(string(bytes.ToValidUTF8(in, []byte("\uFFFD"))), "\uFEFF")
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	s = strings.ReplaceAll(s, "\x00", "")
	lines := strings.Split(s, "\n")
	if h := lines[0]; h != "WEBVTT" && !strings.HasPrefix(h, "WEBVTT ") && !strings.HasPrefix(h, "WEBVTT\t") {
		return nil, 0, errors.New("not WebVTT: no WEBVTT header")
	}
	var cues []vttCue
	var block []string
	flush := func() {
		defer func() { block = block[:0] }()
		if len(block) == 0 {
			return
		}
		t := 0
		if !strings.Contains(block[0], "-->") {
			t = 1
		}
		if t >= len(block) {
			return
		}
		m := vttTiming.FindStringSubmatch(strings.TrimSpace(block[t]))
		if m == nil {
			return // NOTE, STYLE, REGION or garbage
		}
		start, ok1 := vttTime(m[1])
		end, ok2 := vttTime(m[2])
		if !ok1 || !ok2 || end <= start {
			return
		}
		var settings []string
		for _, f := range strings.Fields(m[3]) {
			if vttSetting.MatchString(f) {
				settings = append(settings, f)
			}
		}
		text := cueText(strings.Join(block[t+1:], "\n"))
		if text == "" || assDrawing.MatchString(text) {
			return
		}
		cues = append(cues, vttCue{start: start, end: end, settings: strings.Join(settings, " "), text: text})
	}
	for _, l := range lines[1:] {
		if strings.TrimSpace(l) == "" {
			flush()
			continue
		}
		block = append(block, l)
	}
	flush()
	slices.SortStableFunc(cues, func(a, b vttCue) int { return cmp.Compare(a.start, b.start) })
	var b strings.Builder
	b.WriteString("WEBVTT\n")
	for _, c := range cues {
		fmt.Fprintf(&b, "\n%s --> %s", vttStamp(c.start), vttStamp(c.end))
		if c.settings != "" {
			b.WriteString(" " + c.settings)
		}
		b.WriteString("\n" + c.text + "\n")
	}
	return []byte(b.String()), len(cues), nil
}

// cueText keeps b, i and u (balanced), drops every other tag and escapes
// the text; blank lines and surrounding space go.
func cueText(s string) string {
	var b strings.Builder
	var open []string
	text := func(t string) {
		t = html.UnescapeString(t)
		t = strings.Map(func(r rune) rune {
			if r == '\t' || r == '\n' || !unicode.IsControl(r) {
				return r
			}
			return -1
		}, t)
		b.WriteString(strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(t))
	}
	for s != "" {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			text(s)
			break
		}
		text(s[:i])
		j := strings.IndexByte(s[i:], '>')
		if j < 0 {
			text(s[i:])
			break
		}
		tag := strings.ToLower(s[i+1 : i+j])
		if !vttTag.MatchString(tag) {
			text("<") // a literal "<" (SRT and ASS text is not markup)
			s = s[i+1:]
			continue
		}
		s = s[i+j+1:]
		closing := strings.HasPrefix(tag, "/")
		name := strings.TrimPrefix(tag, "/")
		if k := strings.IndexAny(name, " \t.="); k >= 0 {
			name = name[:k]
		}
		if name != "b" && name != "i" && name != "u" {
			continue
		}
		if !closing {
			open = append(open, name)
			b.WriteString("<" + name + ">")
			continue
		}
		if k := slices.Index(open, name); k >= 0 {
			for n := len(open) - 1; n >= k; n-- {
				b.WriteString("</" + open[n] + ">")
			}
			open = open[:k]
		}
	}
	for n := len(open) - 1; n >= 0; n-- {
		b.WriteString("</" + open[n] + ">")
	}
	var out []string
	for _, l := range strings.Split(b.String(), "\n") {
		if l = strings.TrimSpace(l); strings.TrimSpace(markup.ReplaceAllString(l, "")) != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// vttTime parses a timing's "[hh:]mm:ss.ttt" (shape checked by vttTiming).
func vttTime(s string) (int64, bool) {
	parts := strings.Split(s, ":")
	sec, frac, _ := strings.Cut(parts[len(parts)-1], ".")
	ss, err1 := strconv.Atoi(sec)
	ms, err2 := strconv.Atoi(frac)
	mm, err3 := strconv.Atoi(parts[len(parts)-2])
	hh := 0
	var err4 error
	if len(parts) == 3 {
		hh, err4 = strconv.Atoi(parts[0])
	}
	if errors.Join(err1, err2, err3, err4) != nil || ss > 59 || mm > 59 {
		return 0, false
	}
	return ((int64(hh)*60+int64(mm))*60+int64(ss))*1000 + int64(ms), true
}

func vttStamp(ms int64) string {
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// decodeText returns a sidecar's text as UTF-8: an explicit charset, else a
// BOM (UTF-8/16/32), else UTF-16 by its NUL bytes, else UTF-8 when valid,
// else the legacy charsets of lang (BCP 47) that decode cleanly, else the
// best-scoring common legacy charset (CJK, Cyrillic), else Windows-1252.
func decodeText(b []byte, lang, charset string) (string, error) {
	if charset != "" {
		enc, err := ianaindex.IANA.Encoding(charset)
		if err != nil || enc == nil {
			return "", fmt.Errorf("unknown charset %q", charset)
		}
		return decodeWith(enc, b)
	}
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return string(b[3:]), nil
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE, 0, 0}):
		return decodeWith(utf32.UTF32(utf32.LittleEndian, utf32.ExpectBOM), b)
	case bytes.HasPrefix(b, []byte{0, 0, 0xFE, 0xFF}):
		return decodeWith(utf32.UTF32(utf32.BigEndian, utf32.ExpectBOM), b)
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		return decodeWith(xunicode.UTF16(xunicode.LittleEndian, xunicode.ExpectBOM), b)
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		return decodeWith(xunicode.UTF16(xunicode.BigEndian, xunicode.ExpectBOM), b)
	}
	if e := utf16NoBOM(b); e != nil {
		return decodeWith(e, b)
	}
	if utf8.Valid(b) {
		return string(b), nil
	}
	for _, c := range langCharsets(lang) {
		if s, err := decodeWith(c.enc, b); err == nil && !strings.ContainsRune(s, utf8.RuneError) {
			return s, nil
		}
	}
	best, bestScore := "", 0.0
	for _, c := range legacyCharsets {
		if c.pairs != nil && pairShare(b, c.pairs) < 0.9 {
			continue // accented Latin, not this double-byte charset
		}
		s, err := decodeWith(c.enc, b)
		if err != nil || strings.ContainsRune(s, utf8.RuneError) {
			continue
		}
		if sc := c.score(s); sc > bestScore {
			best, bestScore = s, sc
		}
	}
	if bestScore >= 0.9 {
		return best, nil
	}
	return decodeWith(charmap.Windows1252, b)
}

func decodeWith(e textenc.Encoding, b []byte) (string, error) {
	out, err := e.NewDecoder().Bytes(b)
	return string(out), err
}

// utf16NoBOM detects BOM-less UTF-16 by NULs in one byte of each pair.
func utf16NoBOM(b []byte) textenc.Encoding {
	n := min(len(b), 4096) &^ 1
	if n < 8 {
		return nil
	}
	var even, odd int
	for i := 0; i < n; i += 2 {
		if b[i] == 0 {
			even++
		}
		if b[i+1] == 0 {
			odd++
		}
	}
	switch pairs := n / 2; {
	case odd > pairs*3/10 && even < pairs/20:
		return xunicode.UTF16(xunicode.LittleEndian, xunicode.IgnoreBOM)
	case even > pairs*3/10 && odd < pairs/20:
		return xunicode.UTF16(xunicode.BigEndian, xunicode.IgnoreBOM)
	}
	return nil
}

type legacyCharset struct {
	enc   textenc.Encoding
	score func(string) float64 // share of the text consistent with the charset's script
	// pairs reports a lead and trail byte in the charset's common range
	// (kana, Hangul, frequent Han); nil for single-byte charsets.
	pairs func(lead, trail byte) bool
}

// pairShare is the share of bytes above 0x7F that pair as common
// double-byte characters. Windows-1252 accents rarely do: "Für Größe" is
// FC 72 (an ASCII trail), F6 DF (a rare Han lead).
func pairShare(b []byte, pair func(lead, trail byte) bool) float64 {
	var high, paired int
	for i := 0; i < len(b); i++ {
		if b[i] < 0x80 {
			continue
		}
		if i+1 < len(b) && pair(b[i], b[i+1]) {
			high, paired = high+2, paired+2
			i++
			continue
		}
		high++
	}
	if high == 0 {
		return 0
	}
	return float64(paired) / float64(high)
}

func in(b, lo, hi byte) bool { return b >= lo && b <= hi }

func scriptShare(s string, in func(rune) bool, need func(rune) bool, ofLetters bool) float64 {
	var total, hit int
	found := need == nil
	for _, r := range s {
		if r < 0x80 {
			if ofLetters && unicode.IsLetter(r) {
				total++
			}
			continue
		}
		if unicode.IsSpace(r) || unicode.IsPunct(r) && r < 0x3000 {
			continue
		}
		total++
		if in(r) {
			hit++
		}
		if need != nil && need(r) {
			found = true
		}
	}
	if total == 0 || !found {
		return 0
	}
	return float64(hit) / float64(total)
}

func isKana(r rune) bool { return r >= 0x3040 && r <= 0x30FF }
func isJapanese(r rune) bool {
	return isKana(r) || unicode.Is(unicode.Han, r) || r >= 0x3000 && r <= 0x303F || r >= 0xFF01 && r <= 0xFF5E
}
func isChinese(r rune) bool {
	return unicode.Is(unicode.Han, r) || r >= 0x3000 && r <= 0x303F || r >= 0xFF01 && r <= 0xFF5E
}
func isHangul(r rune) bool   { return unicode.Is(unicode.Hangul, r) || r >= 0x3000 && r <= 0x303F }
func isCyrillic(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }

// legacyCharsets are tried without a language hint, in this order on ties.
// Without a language hint a CJK charset needs its common-range pairs (and
// kana for Japanese): else Windows-1252.
var legacyCharsets = []legacyCharset{
	{japanese.ShiftJIS, func(s string) float64 { return scriptShare(s, isJapanese, isKana, false) },
		func(l, t byte) bool {
			return (in(l, 0x81, 0x9F) || in(l, 0xE0, 0xEF)) && (in(t, 0x40, 0x7E) || in(t, 0x80, 0xFC))
		}},
	{japanese.EUCJP, func(s string) float64 { return scriptShare(s, isJapanese, isKana, false) },
		func(l, t byte) bool { return in(l, 0xA1, 0xFE) && in(t, 0xA1, 0xFE) || l == 0x8E && in(t, 0xA1, 0xDF) }},
	{korean.EUCKR, func(s string) float64 { return scriptShare(s, isHangul, nil, false) },
		func(l, t byte) bool { return in(l, 0xB0, 0xC8) && in(t, 0xA1, 0xFE) || l == 0xA1 && in(t, 0xA1, 0xFE) }},
	// GB2312 level 1 (the 3,755 frequent Han) and its punctuation row.
	{simplifiedchinese.GB18030, func(s string) float64 { return scriptShare(s, isChinese, nil, false) },
		func(l, t byte) bool { return (in(l, 0xB0, 0xD7) || l == 0xA1 || l == 0xA3) && in(t, 0xA1, 0xFE) }},
	// Big5 level 1 (frequent Han) and its symbols.
	{traditionalchinese.Big5, func(s string) float64 { return scriptShare(s, isChinese, nil, false) },
		func(l, t byte) bool { return in(l, 0xA1, 0xC6) && (in(t, 0x40, 0x7E) || in(t, 0xA1, 0xFE)) }},
	{charmap.Windows1251, func(s string) float64 { return scriptShare(s, isCyrillic, nil, true) }, nil},
}

// langCharsets are a language's legacy subtitle charsets, most likely first.
func langCharsets(lang string) []legacyCharset {
	tag, err := language.Parse(lang)
	if err != nil {
		return nil
	}
	base, _ := tag.Base()
	script, _ := tag.Script()
	region, _ := tag.Region()
	encs := map[string][]textenc.Encoding{
		"ja": {japanese.ShiftJIS, japanese.EUCJP, japanese.ISO2022JP},
		"ko": {korean.EUCKR},
		"zh": {simplifiedchinese.GB18030, traditionalchinese.Big5},
		"ru": {charmap.Windows1251, charmap.KOI8R}, "uk": {charmap.Windows1251, charmap.KOI8U}, "be": {charmap.Windows1251},
		"bg": {charmap.Windows1251}, "sr": {charmap.Windows1251, charmap.Windows1250}, "mk": {charmap.Windows1251},
		"el": {charmap.Windows1253}, "tr": {charmap.Windows1254}, "he": {charmap.Windows1255},
		"ar": {charmap.Windows1256}, "fa": {charmap.Windows1256}, "vi": {charmap.Windows1258}, "th": {charmap.Windows874},
		"pl": {charmap.Windows1250}, "cs": {charmap.Windows1250}, "sk": {charmap.Windows1250}, "hu": {charmap.Windows1250},
		"ro": {charmap.Windows1250}, "hr": {charmap.Windows1250}, "sl": {charmap.Windows1250},
	}[base.String()]
	if base.String() == "zh" && (script.String() == "Hant" || slices.Contains([]string{"TW", "HK", "MO"}, region.String())) {
		encs = []textenc.Encoding{traditionalchinese.Big5, simplifiedchinese.GB18030}
	}
	out := make([]legacyCharset, len(encs))
	for i, e := range encs {
		out[i] = legacyCharset{enc: e}
	}
	return out
}
