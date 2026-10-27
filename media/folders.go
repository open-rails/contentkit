package media

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/contentkit/media/layout"
)

// ErrFolderNotEmpty: a new item's folder already holds objects. Item ids are
// never reused, so leftovers mean a host bug (a reset id, a restored
// database); they are never adopted. Purge them deliberately with Jobs.Purge.
var ErrFolderNotEmpty = errors.New("media: new item's folder is not empty")

// FolderNotEmptyError names the folder and a few of its keys.
type FolderNotEmptyError struct {
	Prefix string
	Keys   []string
}

func (e *FolderNotEmptyError) Error() string {
	return fmt.Sprintf("%v: %s holds %s", ErrFolderNotEmpty, e.Prefix, strings.Join(e.Keys, ", "))
}
func (e *FolderNotEmptyError) Unwrap() error { return ErrFolderNotEmpty }

// requireFresh backs a folder's first manifest: public files with no
// manifest are a previous item's leftovers (uploads may precede a first
// commit; public files never do).
func (m *Manifests) requireFresh(ctx context.Context, item Item) error {
	keys, err := m.keys(ctx, item, item.PublicPrefix(), 3)
	if err != nil {
		return err
	}
	if len(keys) > 0 {
		return &FolderNotEmptyError{Prefix: item.Prefix(), Keys: keys}
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
