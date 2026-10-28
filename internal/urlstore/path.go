package urlstore

import (
	"strings"

	contenturl "github.com/open-rails/contentkit/contenturl"
)

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
// Prefix(languages, language); slug is the caller's decorative path for that
// language (an entity and a title, "/"-joined). Every path ContentKit answers
// is built here.
func Path(languages []string, language, route string, code contenturl.Code, slug string) string {
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

// Canonical returns link's canonical path in language (the default when
// ""), with its slug there; false when link's kind has no route.
func Canonical(routes contenturl.Routes, languages []string, language string, link contenturl.Link) (string, bool) {
	route, ok := routes[link.ContentKind]
	if !ok {
		return "", false
	}
	language = Prefix(languages, language)
	return Path(languages, language, route, link.Code, link.SlugFor(language)), true
}
