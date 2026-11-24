package contentstore

import (
	"context"
	"strings"
)

// trimProcessor is the default TextProcessor: trim plain text.
type trimProcessor struct{}

func (trimProcessor) ProcessText(_ context.Context, raw string) (string, error) {
	return strings.TrimSpace(raw), nil
}
