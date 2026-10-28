package contenturl

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/open-rails/contentkit/contentref"
)

// Link is what a code resolves to: the content, its canonical code and slugs.
// After a merge the content and code are the surviving record's.
type Link struct {
	contentref.ContentRef
	Code  Code              `json:"code"`
	Slug  string            `json:"slug"`
	Slugs map[string]string `json:"slugs,omitempty"`
}

// SlugFor returns the slug for language, falling back to the default slug.
func (l Link) SlugFor(language string) string {
	if s := l.Slugs[language]; s != "" {
		return s
	}
	return l.Slug
}

// Routes maps a content kind to the host's route: the first path segment of
// its pages ("video" -> "watch", "gallery" -> "g"). Several kinds may share a
// route; a kind without one has no URL.
type Routes map[string]string

var segmentRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Validate checks every route and language is one lowercase path segment and
// that no language is also a route.
func (r Routes) Validate(languages []string) error {
	if len(r) == 0 {
		return fmt.Errorf("%w: Routes is empty", ErrInvalid)
	}
	routes := map[string]bool{}
	for kind, route := range r {
		if strings.TrimSpace(kind) == "" || !segmentRE.MatchString(route) {
			return fmt.Errorf("%w: route %q for kind %q must be one lowercase path segment", ErrInvalid, route, kind)
		}
		routes[route] = true
	}
	for _, l := range languages {
		if !segmentRE.MatchString(l) {
			return fmt.Errorf("%w: language %q must be one lowercase path segment", ErrInvalid, l)
		}
		if routes[l] {
			return fmt.Errorf("%w: language %q is also a route", ErrInvalid, l)
		}
	}
	return nil
}

func (r Routes) has(route string) bool {
	for _, v := range r {
		if v == route {
			return true
		}
	}
	return false
}

// AcceptLanguage is the first of languages an Accept-Language header names,
// as a tag or a tag's primary subtag; "" when it names none.
func AcceptLanguage(languages []string, header string) string {
	for _, part := range strings.Split(header, ",") {
		tag, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		tag = strings.ToLower(tag)
		for _, l := range languages {
			if tag == l || strings.HasPrefix(tag, l+"-") {
				return l
			}
		}
	}
	return ""
}

// Parsed is a content path split into its parts.
type Parsed struct {
	Language string // "" when unprefixed (an old link: it redirects)
	Route    string
	Code     Code   // canonical form
	RawCode  string // as written
	Slug     string // decorative segments as written, "/"-joined; "" when absent
}

// ParsePath splits [/{language}]/{route}/{code}[/{decorative...}][/] where
// route is one of routes' values and language one of languages; a path without
// the language, or with stale or extra decorative segments, is still read, for
// its redirect. ok is false for any other path, including an invalid code.
func ParsePath(path string, routes Routes, languages []string) (Parsed, bool) {
	var p Parsed
	rest, ok := strings.CutPrefix(path, "/")
	if !ok {
		return p, false
	}
	rest = strings.TrimSuffix(rest, "/")
	segs := strings.Split(rest, "/")
	for _, s := range segs {
		if s == "" {
			return p, false
		}
	}
	if len(segs) > 2 {
		for _, l := range languages {
			if segs[0] == l {
				p.Language, segs = l, segs[1:]
				break
			}
		}
	}
	// A stray first segment before a known route, such as a language the site
	// does not serve, is dropped: the path redirects to its canonical language.
	if p.Language == "" && len(segs) > 2 && !routes.has(segs[0]) && routes.has(segs[1]) {
		segs = segs[1:]
	}
	if len(segs) < 2 || !routes.has(segs[0]) {
		return Parsed{}, false
	}
	code, err := ParseCode(segs[1])
	if err != nil {
		return Parsed{}, false
	}
	p.Route, p.Code, p.RawCode = segs[0], code, segs[1]
	p.Slug = strings.Join(segs[2:], "/")
	return p, true
}
