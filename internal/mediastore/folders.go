package mediastore

import (
	"context"

	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// requireFresh backs a folder's first manifest: public files with no
// manifest are a previous item's leftovers (uploads may precede a first
// commit; public files never do).
func (m *Manifests) requireFresh(ctx context.Context, item Item) error {
	keys, err := m.keys(ctx, item, item.PublicPrefix(), 3)
	if err != nil {
		return err
	}
	if len(keys) > 0 {
		return &media.FolderNotEmptyError{Prefix: item.Prefix(), Keys: keys}
	}
	return nil
}

// keys lists up to n of item's own keys under prefix.
func (m *Manifests) keys(ctx context.Context, item Item, prefix string, n int) ([]string, error) {
	var keys []string
	for o, err := range m.store.List(ctx, prefix) {
		if err != nil {
			return nil, err
		}
		if k, ok := layout.Parse(o.Key); ok && k.Folder() != item.Prefix() {
			continue // a version's folder within a content's
		}
		if keys = append(keys, o.Key); len(keys) == n {
			break
		}
	}
	return keys, nil
}
