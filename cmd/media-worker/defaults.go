package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/avatar"
	"github.com/open-rails/contentkit/media/image"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/worker"
)

// publishDefaults renders the images the kinds' public presets name
// (Public.Default) to their create-only objects, then prints each new key
// and the gateway's MEDIA_GATEWAY_DEFAULTS. Deploy that value only after
// it succeeds.
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
	reg, err := media.NewRegistry(kinds)
	if err != nil {
		return fmt.Errorf("MEDIA_KINDS_FILE: %w", err)
	}
	store, err := worker.StoreFromEnv()
	if err != nil {
		return err
	}
	// Learns the bucket's conditional-PUT support, which create-only writes need.
	if err := store.Check(ctx); err != nil {
		return fmt.Errorf("media store: %w", err)
	}
	published, publishErr := image.PublishDefaults(ctx, store, reg)
	// Report what was written even when publication stopped part way.
	for _, key := range published.Written {
		if _, err := fmt.Fprintln(out, key); err != nil {
			return errors.Join(publishErr, err)
		}
	}
	if publishErr != nil {
		return publishErr
	}
	_, err = fmt.Fprintf(out, "Published %d default rendition(s).\nMEDIA_GATEWAY_DEFAULTS=%s\n",
		len(published.Written), layout.FormatDefaults(published.Defaults))
	return err
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
