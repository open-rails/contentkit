package catalogstore

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"

	catalog "github.com/open-rails/contentkit/catalog"
)

var (
	ErrInvalid = errors.New("catalog: invalid request")
)

var (
	ErrNotFound = errors.New("catalog: not found")
)

var (
	ErrForbidden = errors.New("catalog: forbidden")
)

var (
	ErrConflict = errors.New("catalog: conflict")
)

// DefaultVersion is the version a reader of language opens by default, the
// one rule cards and contentkit-ui's defaultVersion follow (internal/edition):
// the first live version serving language, else the first live one, else
// the first; vs in position order. Nil when vs is empty.
func DefaultVersion(vs []catalog.Version, language string) *catalog.Version {
	rank := func(v *catalog.Version) int {
		r := 0
		if v.Live {
			r += 2
		}
		if language != "" && slices.Contains(v.Languages, language) {
			r++
		}
		return r
	}
	var best *catalog.Version
	for i := range vs {
		if best == nil || rank(&vs[i]) > rank(best) {
			best = &vs[i]
		}
	}
	return best
}

var imageRefRe = regexp.MustCompile(catalog.ImageScheme + `(i-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

// ImageRef is the body reference to an inline image name ("i-{uuid}").
func ImageRef(name string) string { return catalog.ImageScheme + name }

func imageNames(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range imageRefRe.FindAllStringSubmatch(body, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

func resolveBody(body string, urls map[string]string) string {
	if len(urls) == 0 {
		return body
	}
	return imageRefRe.ReplaceAllStringFunc(body, func(ref string) string {
		if u, ok := urls[ref[len(catalog.ImageScheme):]]; ok {
			return u
		}
		return ref
	})
}

type stripTags struct{}

var tagRe = regexp.MustCompile(`<[^>]*>`)

func (stripTags) Sanitize(_ context.Context, raw string) (string, error) {
	return strings.TrimSpace(tagRe.ReplaceAllString(raw, "")), nil
}
