package searchkit

import (
	"strings"
	"unicode"
)

func isASCIIOnlyQuery(q string) bool {
	q = strings.TrimSpace(q)
	if q == "" {
		return true
	}
	for i := 0; i < len(q); i++ {
		if q[i] >= 0x80 {
			return false
		}
	}
	return true
}

func normalizeWhitespace(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	return strings.Join(strings.Fields(q), " ")
}

func hasAnyLetterOrNumber(q string) bool {
	for _, r := range q {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return true
		}
	}
	return false
}
