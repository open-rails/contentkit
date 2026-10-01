package media

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/contentkit/contentref"
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

// Create starts a new item: it writes the item's empty manifest, hidden
// until the first commit or Expose resolves it, and fails with
// ErrFolderNotEmpty if the folder already holds any object. Hosts call it
// when they create the item's row, so a reused id surfaces there.
func (m *Manifests) Create(ctx context.Context, ref contentref.ContentRef) (*Manifest, error) {
	item, err := m.reg.Item(ref)
	if err != nil {
		return nil, err
	}
	if keys, err := m.keys(ctx, item.Prefix(), 3); err != nil {
		return nil, err
	} else if len(keys) > 0 {
		return nil, &FolderNotEmptyError{Prefix: item.Prefix(), Keys: keys}
	}
	return m.Edit(ctx, ref, func(m *Manifest) error { m.Hidden = true; return nil })
}

// requireFresh backs a folder's first manifest: public files with no
// manifest are a previous item's leftovers (uploads may precede a first
// commit; public files never do).
func (m *Manifests) requireFresh(ctx context.Context, item Item) error {
	keys, err := m.keys(ctx, item.PublicPrefix(), 3)
	if err != nil {
		return err
	}
	if len(keys) > 0 {
		return &FolderNotEmptyError{Prefix: item.Prefix(), Keys: keys}
	}
	return nil
}

func (m *Manifests) keys(ctx context.Context, prefix string, n int) ([]string, error) {
	var keys []string
	for o, err := range m.store.List(ctx, prefix) {
		if err != nil {
			return nil, err
		}
		if keys = append(keys, o.Key); len(keys) == n {
			break
		}
	}
	return keys, nil
}
