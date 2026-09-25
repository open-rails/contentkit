// Command media-worker is the stock media worker (media/worker) for hosts
// whose kinds are plain data: it reads them from a JSON file. Hosts with a
// per-file spec chooser or hooks (Failed, SlotEncoded) build their own worker
// from the code that builds their media.Registry instead.
//
// Environment: worker.FromEnv and Config.TuningFromEnv, plus
//
//	MEDIA_KINDS_FILE   JSON array of media.Kind, the host's registry (e.g. [{"Name":"clip","Video":{}}])
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/worker"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("media-worker", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	kinds, err := loadKinds(os.Getenv("MEDIA_KINDS_FILE"))
	if err != nil {
		return err
	}
	cfg, err := worker.FromEnv(ctx)
	if err != nil {
		return err
	}
	defer cfg.Pool.Close()
	cfg.Kinds, cfg.Logger = kinds, log
	w, err := worker.New(ctx, cfg)
	if err != nil {
		return err
	}
	return w.Run(ctx)
}

func loadKinds(path string) (*media.Registry, error) {
	if path == "" {
		return nil, fmt.Errorf("MEDIA_KINDS_FILE is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("MEDIA_KINDS_FILE: %w", err)
	}
	var kinds []media.Kind
	if err := json.Unmarshal(b, &kinds); err != nil {
		return nil, fmt.Errorf("MEDIA_KINDS_FILE: %w", err)
	}
	return media.NewRegistry(kinds...)
}
