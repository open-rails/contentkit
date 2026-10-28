package urlstore

import (
	"fmt"
	"strings"

	contenturl "github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/codes"
)

var (
	ErrNotFound = codes.ErrNotFound
)

var (
	ErrConflict = codes.ErrConflict
)

// parseCrockford reads n Crockford characters, at least one a letter.
func parseCrockford(s string, n int) (string, bool) {
	b := make([]byte, 0, n)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '-':
			continue
		case c >= 'a' && c <= 'z':
			c -= 'a' - 'A'
		}
		switch c {
		case 'O':
			c = '0'
		case 'I', 'L':
			c = '1'
		}
		if strings.IndexByte(contenturl.Alphabet, c) < 0 || len(b) == n {
			return "", false
		}
		b = append(b, c)
	}
	if len(b) != n || strings.IndexFunc(string(b), func(r rune) bool { return r >= 'A' && r <= 'Z' }) < 0 {
		return "", false
	}
	return string(b), true
}

// ErrInvalidVersionCode refuses what is not a version code.
var ErrInvalidVersionCode = fmt.Errorf("%w: not a version code", codes.ErrInvalid)

// ParseVersionCode reads a version code with ParseCode's rules.
func ParseVersionCode(s string) (contenturl.VersionCode, error) {
	c, ok := parseCrockford(s, contenturl.VersionCodeLength)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrInvalidVersionCode, s)
	}
	return contenturl.VersionCode(c), nil
}

// Versioned is path with ?v=code selecting a version; "" leaves path as is.
func Versioned(path string, code contenturl.VersionCode) string {
	if code == "" {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + contenturl.VersionParam + "=" + string(code)
}

// ValidSlug reports whether s is a slug Slugify can produce ("" included).
func ValidSlug(s string) bool { return codes.ValidSlug(s) }
