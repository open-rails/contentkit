package contentstore

import (
	"context"
	"strings"
)

// trimProcessor is the default ContentProcessor: trim plain text.
type trimProcessor struct{}

func (trimProcessor) Sanitize(_ context.Context, raw string) (string, error) {
	return strings.TrimSpace(raw), nil
}
