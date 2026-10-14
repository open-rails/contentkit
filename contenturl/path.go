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

// Prefix is the language of a page asked for in language on a site of
// languages, its default first: language, else the default. Every content
// page is under its language; contentkit-ui's languagePrefix mirrors this.
func Prefix(languages []string, language string) string {
	if language == "" && len(languages) > 0 {
		return languages[0]
	}
	return language
}

// Path is a content page's path, /{language}/{route}/{code}[/{slug}], under
// Prefix(languages, language); slug is the caller's for that language. Every
// path ContentKit answers is built here.
func Path(languages []string, language, route string, code Code, slug string) string {
	var b strings.Builder
	if l := Prefix(languages, language); l != "" {
		b.WriteString("/" + l)
	}
	b.WriteString("/" + route + "/" + string(code))
	if slug != "" {
		b.WriteString("/" + slug)
	}
	return b.String()
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
	Slug     string // as written; "" when absent
}

// ParsePath splits [/{language}]/{route}/{code}[/{slug}][/] where route is one
// of routes' values and language one of languages; a path without the
// language is still read, for its redirect. ok is false for any other path,
// including an invalid code.
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
	if len(segs) < 2 || len(segs) > 3 || !routes.has(segs[0]) {
		return Parsed{}, false
	}
	code, err := ParseCode(segs[1])
	if err != nil {
		return Parsed{}, false
	}
	p.Route, p.Code, p.RawCode = segs[0], code, segs[1]
	if len(segs) == 3 {
		p.Slug = segs[2]
	}
	return p, true
}

// Canonical returns link's canonical path in language (the default when
// ""), with its slug there; false when link's kind has no route.
func Canonical(routes Routes, languages []string, language string, link Link) (string, bool) {
	route, ok := routes[link.ContentKind]
	if !ok {
		return "", false
	}
	language = Prefix(languages, language)
	return Path(languages, language, route, link.Code, link.SlugFor(language)), true
}
