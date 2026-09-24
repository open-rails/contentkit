package content

import (
	"context"
	"regexp"
	"strings"
)

// noopEnricher returns no display data; handlers fall back to bare ids.
type noopEnricher struct{}

func (noopEnricher) UsersByIDs(context.Context, []string) (map[string]PublicUser, error) {
	return map[string]PublicUser{}, nil
}

// stripProcessor is the default ContentProcessor: strip HTML tags to plain text.
type stripProcessor struct{}

var tagRe = regexp.MustCompile(`<[^>]*>`)

func (stripProcessor) Sanitize(_ context.Context, raw string) (string, error) {
	return strings.TrimSpace(tagRe.ReplaceAllString(raw, "")), nil
}
