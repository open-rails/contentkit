package content

import (
	"context"
	"regexp"
	"strings"
)

// stripProcessor is the default ContentProcessor: strip HTML tags to plain text.
type stripProcessor struct{}

var tagRe = regexp.MustCompile(`<[^>]*>`)

func (stripProcessor) Sanitize(_ context.Context, raw string) (string, error) {
	return strings.TrimSpace(tagRe.ReplaceAllString(raw, "")), nil
}
