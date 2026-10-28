package catalogstore

import (
	"context"
	"strings"
	"unicode/utf8"

	catalog "github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/kinds"
	"golang.org/x/text/language"
)

func cleanOriginalTitle(title, sourceLanguage string) (string, string, error) {
	title, sourceLanguage = strings.TrimSpace(title), strings.TrimSpace(sourceLanguage)
	if utf8.RuneCountInString(title) > maxTitle {
		return "", "", invalid("original title exceeds %d characters", maxTitle)
	}
	if len(sourceLanguage) > 128 {
		return "", "", invalid("original language is too long")
	}
	if sourceLanguage != "" {
		tag, err := language.Parse(sourceLanguage)
		if err != nil {
			return "", "", invalid("original language %q is not a language tag", sourceLanguage)
		}
		sourceLanguage = tag.String()
	}
	return title, sourceLanguage, nil
}

// prepareVersionText validates and sanitizes content before a SQL transaction
// holds a connection. Editors can start an empty version and fill it later.
func (s *Store) prepareVersionText(ctx context.Context, v catalog.VersionInput) (catalog.VersionInput, error) {
	if v.Text != nil {
		clean := make(map[string]catalog.VersionText, len(v.Text))
		for l, t := range v.Text {
			x, err := s.cleanText(ctx, l, t)
			if err != nil {
				return v, err
			}
			clean[l] = x
		}
		v.Text = clean
	}
	if v.OriginalTitle != nil {
		x, _, err := cleanOriginalTitle(*v.OriginalTitle, "")
		if err != nil {
			return v, err
		}
		v.OriginalTitle = &x
	}
	if v.OriginalLanguage != nil {
		_, x, err := cleanOriginalTitle("", *v.OriginalLanguage)
		if err != nil {
			return v, err
		}
		v.OriginalLanguage = &x
	}
	return v, nil
}

// versionLanguage resolves within this edition only. The preferred declared
// language wins, then a configured language stored on the same edition.
func (s *Store) versionLanguage(v *catalog.Version, requested string) string {
	if requested != "" {
		if _, ok := v.Text[requested]; ok {
			return requested
		}
	}
	for _, l := range v.Languages {
		if _, ok := v.Text[l]; ok {
			return l
		}
	}
	for _, l := range s.cfg.Languages {
		if _, ok := v.Text[l]; ok {
			return l
		}
	}
	if len(v.Languages) > 0 {
		return v.Languages[0]
	}
	return s.defaultLanguage()
}

func (s *Store) resolveVersionText(kind string, v *catalog.Version, requested string) {
	v.Language = s.versionLanguage(v, requested)
	t := v.Text[v.Language]
	v.Title, v.Description, v.Body, v.Slug = t.Title, t.Description, t.Body, t.Slug
	if s.cfg.Kinds[kind].TitlePolicy == kinds.TitleOriginalAndVersion && v.Title == "" {
		v.Title = v.OriginalTitle
	}
}
