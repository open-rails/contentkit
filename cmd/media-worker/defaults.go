package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/open-rails/contentkit/internal/media/image"
	"github.com/open-rails/contentkit/internal/media/worker"
	"github.com/open-rails/contentkit/media/avatar"
)

// publishDefaults renders the images the kinds' public presets name
// (Public.Default) into each kind's _default item and its manifest, then
// prints the kinds it wrote. Rendering again changes nothing.
func publishDefaults(ctx context.Context, out io.Writer) error {
	kinds, err := readKinds(os.Getenv("MEDIA_KINDS_FILE"))
	if err != nil {
		return err
	}
	images := layered{avatar.Defaults()}
	if dir := os.Getenv("MEDIA_DEFAULTS_DIR"); dir != "" {
		images = append(layered{os.DirFS(dir)}, images...)
	}
	for i := range kinds.Kinds {
		kinds.Kinds[i].Defaults = images
	}
	reg, err := mediastore.NewRegistry(kinds, mediastore.Hooks{})
	if err != nil {
		return fmt.Errorf("MEDIA_KINDS_FILE: %w", err)
	}
	store, err := worker.StoreFromEnv()
	if err != nil {
		return err
	}
	// Learns the bucket's capabilities, which manifest writes need.
	if err := store.Check(ctx); err != nil {
		return fmt.Errorf("media store: %w", err)
	}
	manifests, err := mediastore.NewManifests(store, reg, mediastore.ManifestOptions{})
	if err != nil {
		return err
	}
	kindsWritten, publishErr := image.PublishDefaults(ctx, manifests)
	// Report what was written even when publication stopped part way.
	for _, kind := range kindsWritten {
		if _, err := fmt.Fprintln(out, kind); err != nil {
			return errors.Join(publishErr, err)
		}
	}
	return publishErr
}

// layered opens a name from the first file system that has it.
type layered []fs.FS

func (l layered) Open(name string) (fs.File, error) {
	for _, fsys := range l {
		if f, err := fsys.Open(name); !errors.Is(err, fs.ErrNotExist) {
			return f, err
		}
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}
