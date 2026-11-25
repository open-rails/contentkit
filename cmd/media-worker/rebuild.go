package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/open-rails/contentkit/internal/media/worker"
)

// rebuild refills the item cache from the bucket and prints the count.
func rebuild(ctx context.Context, log *slog.Logger) error {
	declared, err := readKinds(os.Getenv("MEDIA_KINDS_FILE"))
	if err != nil {
		return err
	}
	kinds, err := mediastore.NewRegistry(declared, mediastore.Hooks{})
	if err != nil {
		return fmt.Errorf("MEDIA_KINDS_FILE: %w", err)
	}
	cfg, err := worker.FromEnv(ctx)
	if err != nil {
		return err
	}
	defer cfg.Pool.Close()
	cfg.Kinds, cfg.Logger = kinds, log
	n, err := worker.Rebuild(ctx, cfg)
	log.Info("media-worker: rebuild", "items", n)
	return err
}
