// Package normalize cleans user query text before retrieval.
package normalize

import (
	"strings"
	"unicode"
)

func isLetterOrNumber(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

// normalizeIntraTokenHyphens replaces '-' between two letters/numbers with a
// space so "two-factor" reads as "two factor".
func normalizeIntraTokenHyphens(s string) string {
	rs := []rune(s)
	if len(rs) == 0 {
		return s
	}
	for i := 1; i < len(rs)-1; i++ {
		if rs[i] != '-' {
			continue
		}
		if isLetterOrNumber(rs[i-1]) && isLetterOrNumber(rs[i+1]) {
			rs[i] = ' '
		}
	}
	return string(rs)
}

func stripLeadingHyphens(tok string) string {
	for strings.HasPrefix(tok, "-") {
		tok = strings.TrimPrefix(tok, "-")
	}
	return tok
}

// Query returns the cleaned query text: hyphenated tokens split, leading '-'
// treated as punctuation, whitespace collapsed. Words such as "not" stay
// plain text; ContentKit has no Boolean query syntax.
func Query(input string) string {
	q := normalizeIntraTokenHyphens(strings.TrimSpace(input))
	if q == "" {
		return ""
	}
	parts := strings.Fields(q)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = stripLeadingHyphens(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, " ")
}

// HasAnyLetterOrNumber reports whether q contains searchable text.
func HasAnyLetterOrNumber(q string) bool {
	for _, r := range q {
		if isLetterOrNumber(r) {
			return true
		}
	}
	return false
}
