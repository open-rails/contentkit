package media

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/open-rails/contentkit/media/layout"
)

// maxNameBytes caps a cleaned template value, in the manifest's JSON bytes.
const maxNameBytes = 200

// CleanName makes a template value ({name}, {title}) safe to fill a path or
// a download name: "/", "\", control characters and ".." are removed, the
// text is NFC-normalized, trimmed and capped at 200 bytes of JSON.
func CleanName(s string) string {
	s = norm.NFC.String(s)
	s = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	for strings.Contains(s, "..") {
		s = strings.ReplaceAll(s, "..", "")
	}
	s = strings.TrimSpace(s)
	for jsonLen(s) > maxNameBytes {
		_, size := utf8.DecodeLastRuneInString(s)
		s = strings.TrimSpace(s[:len(s)-size])
	}
	return s
}

// splitExt splits an upload path into its stem and lower-case extension
// ("originals/001.PNG" is "originals/001", "png"); ext is "" when the last
// segment has none of 1-10 ASCII letters and digits.
func splitExt(p string) (stem, ext string) {
	i := strings.LastIndexByte(p, '.')
	if i <= strings.LastIndexByte(p, '/')+1 || len(p)-i-1 > 10 || i == len(p)-1 {
		return p, ""
	}
	for _, c := range p[i+1:] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return p, ""
		}
	}
	return p[:i], strings.ToLower(p[i+1:])
}

// typeExt is the extension a file of contentType gets when its path has none.
func typeExt(contentType string) string {
	switch contentType {
	case "image/jpeg":
		return "jpg"
	case "image/svg+xml":
		return "svg"
	case "video/quicktime":
		return "mov"
	case "video/x-matroska":
		return "mkv"
	case "audio/mpeg":
		return "mp3"
	case "audio/mp4", "audio/aac":
		return "m4a"
	case "application/x-subrip":
		return "srt"
	case "text/x-ssa":
		return "ssa"
	case "text/x-ass":
		return "ass"
	case "text/vtt":
		return "vtt"
	}
	if _, sub, ok := strings.Cut(contentType, "/"); ok && layout.ValidSegment(sub) && len(sub) <= 10 && !strings.ContainsAny(sub, ".-_") {
		return sub
	}
	return "bin"
}

// pattern is an upload Path or a To template's source pattern: a literal
// stem, or a directory followed by {name}.
type pattern struct {
	dir     string // "originals/" ("" for a top-level pattern)
	literal string // the whole stem of a literal
}

const nameVar = "{name}"

func parsePattern(p string) (pattern, bool) {
	if dir, ok := strings.CutSuffix(p, nameVar); ok {
		return pattern{dir: dir}, (dir == "" || strings.HasSuffix(dir, "/") && validDir(dir))
	}
	return pattern{literal: p}, validDir(p + "/")
}

// validDir accepts "" or "a/b/": ASCII segments.
func validDir(d string) bool {
	if d == "" {
		return true
	}
	segs := strings.Split(strings.TrimSuffix(d, "/"), "/")
	for _, s := range segs {
		if !layout.ValidSegment(s) {
			return false
		}
	}
	return strings.HasSuffix(d, "/")
}

// match returns the {name} of stem: a literal's last segment.
func (p pattern) match(stem string) (string, bool) {
	if p.literal != "" {
		return p.literal[strings.LastIndexByte(p.literal, '/')+1:], stem == p.literal
	}
	name, ok := strings.CutPrefix(stem, p.dir)
	return name, ok && name != "" && CleanName(name) == name
}

// String is the pattern as declared.
func (p pattern) String() string {
	if p.literal != "" {
		return p.literal
	}
	return p.dir + nameVar
}

// fill replaces each {key} in template with vars[key] ("" when missing).
func fill(template string, vars map[string]string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(template, '{')
		j := strings.IndexByte(template[i+1:], '}')
		if i < 0 || j < 0 {
			b.WriteString(template)
			return b.String()
		}
		b.WriteString(template[:i])
		b.WriteString(vars[template[i+1:i+1+j]])
		template = template[i+j+2:]
	}
}

// placeholders lists the {keys} of template.
func placeholders(template string) []string {
	var out []string
	for {
		i := strings.IndexByte(template, '{')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(template[i:], '}')
		if j < 0 {
			return append(out, "")
		}
		out = append(out, template[i+1:i+j])
		template = template[i+j+1:]
	}
}

// metaVars are the manifest's meta values as template strings.
func metaVars(meta map[string]any) map[string]string {
	vars := make(map[string]string, len(meta)+2)
	for k, v := range meta {
		switch v := v.(type) {
		case string:
			vars[k] = CleanName(v)
		case float64:
			vars[k] = strconv.FormatFloat(v, 'f', -1, 64)
		case int:
			vars[k] = strconv.Itoa(v)
		}
	}
	return vars
}

// natLess orders names naturally: digit runs compare by value ("2" < "10").
func natLess(a, b string) bool {
	for a != "" && b != "" {
		da, db := digits(a), digits(b)
		if da > 0 && db > 0 {
			na, nb := strings.TrimLeft(a[:da], "0"), strings.TrimLeft(b[:db], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			if da != db {
				return da < db
			}
			a, b = a[da:], b[db:]
			continue
		}
		ra, sa := utf8.DecodeRuneInString(a)
		rb, sb := utf8.DecodeRuneInString(b)
		if la, lb := unicode.ToLower(ra), unicode.ToLower(rb); la != lb {
			return la < lb
		}
		if ra != rb {
			return ra < rb
		}
		a, b = a[sa:], b[sb:]
	}
	return len(a) < len(b)
}

func digits(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n
}
