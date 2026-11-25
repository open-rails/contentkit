package search

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Request bounds, checked before any retrieval work. Normalization (NFKD) can
// grow text several times, so tokens are bounded after it.
const (
	MaxQueryBytes = 1024
	MaxQueryRunes = 256
	MaxTokens     = 16
	MaxTokenRunes = 128
	// MaxTypoRunes is the longest token that gets one-edit candidates.
	MaxTypoRunes = 16
	// maxTypoBytes bounds one request's typo operand.
	maxTypoBytes = 64 << 10
)

// CheckQuery refuses a query beyond the bounds: ErrInvalidQuery. Callers run
// it before any database work; retrieval checks again.
func CheckQuery(q string) error {
	switch {
	case len(q) > MaxQueryBytes:
		return fmt.Errorf("%w: keyword query exceeds %d bytes", ErrInvalidQuery, MaxQueryBytes)
	case !utf8.ValidString(q) || strings.ContainsRune(q, 0):
		return fmt.Errorf("%w: keyword query is not valid text", ErrInvalidQuery)
	case utf8.RuneCountInString(q) > MaxQueryRunes:
		return fmt.Errorf("%w: keyword query exceeds %d characters", ErrInvalidQuery, MaxQueryRunes)
	}
	return checkTokens(keywordTokens(normalizeQuery(q)))
}

func checkTokens(tokens []string) error {
	if len(tokens) > MaxTokens {
		return fmt.Errorf("%w: keyword query exceeds %d words", ErrInvalidQuery, MaxTokens)
	}
	for _, t := range tokens {
		if utf8.RuneCountInString(t) > MaxTokenRunes {
			return fmt.Errorf("%w: a keyword query word exceeds %d characters", ErrInvalidQuery, MaxTokenRunes)
		}
	}
	return nil
}

// normalizeQuery approximates contentkit_search_normalize: NFKD, combining
// diacritics dropped, NFC, lower case, without the English fillers.
func normalizeQuery(q string) string {
	q = strings.Map(func(r rune) rune {
		if r >= 0x300 && r <= 0x36f {
			return -1
		}
		return r
	}, norm.NFKD.String(q))
	words := strings.Fields(strings.ToLower(norm.NFC.String(q)))
	kept := words[:0]
	for _, w := range words {
		if w != "the" && w != "a" && w != "an" {
			kept = append(kept, w)
		}
	}
	return strings.Join(kept, " ")
}
