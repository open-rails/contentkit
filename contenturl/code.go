// Package contenturl gives content uniform URLs, [/{lang}]/{route}/{CODE}[/{slug}]:
// a short random code ContentKit assigns once per content record (unique per
// tenant across every kind), a decorative slug, and the host's route for the
// content kind. The server reads only the code; any other spelling of the path
// redirects to the canonical one.
//
// Store registers codes and resolves them (and legacy aliases); Router maps
// content kinds to the host's routes, decides canonical redirects and serves a
// JSON lookup. Taxonomy nodes and posts are registered by ContentKit itself;
// hosts Put their own content.
package contenturl

import (
	"fmt"
	"strings"

	"github.com/open-rails/contentkit/internal/codes"
)

// Errors; match with errors.Is.
var (
	ErrInvalid     = codes.ErrInvalid
	ErrNotFound    = codes.ErrNotFound
	ErrConflict    = codes.ErrConflict
	ErrInvalidCode = fmt.Errorf("%w: not a content code", codes.ErrInvalid)
)

// CodeLength is the length of a content code.
const CodeLength = 9

// Alphabet is Crockford base32: digits and uppercase letters without I, L, O, U.
const Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Code is a content code in canonical form: nine characters of Alphabet, at
// least one a letter, so no code reads as a numeric id.
type Code string

func (c Code) String() string { return string(c) }

// ParseCode reads a code with Crockford's decoding rules: any letter case,
// O as 0, I and L as 1, hyphens ignored. U and every other character are
// refused, as are any length but nine and an all-digit result (a legacy
// numeric id such as /watch/346791971 is never a code).
func ParseCode(s string) (Code, error) {
	var b [CodeLength]byte
	n := 0
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
		if strings.IndexByte(Alphabet, c) < 0 || n == CodeLength {
			return "", fmt.Errorf("%w: %q", ErrInvalidCode, s)
		}
		b[n] = c
		n++
	}
	if n != CodeLength || strings.IndexFunc(string(b[:]), func(r rune) bool { return r >= 'A' && r <= 'Z' }) < 0 {
		return "", fmt.Errorf("%w: %q", ErrInvalidCode, s)
	}
	return Code(b[:]), nil
}

// Valid reports whether c is in canonical form.
func (c Code) Valid() bool {
	p, err := ParseCode(string(c))
	return err == nil && p == c
}

// Slugify turns a title into a URL slug: ASCII [a-z0-9] words joined by
// hyphens, diacritics dropped, at most 80 bytes cut at a word boundary. Text
// without Latin letters or digits gives "" (the URL then has no slug).
func Slugify(title string) string { return codes.Slugify(title) }

// ValidSlug reports whether s is a slug Slugify can produce ("" included).
func ValidSlug(s string) bool { return codes.ValidSlug(s) }
