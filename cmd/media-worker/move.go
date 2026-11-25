package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/open-rails/contentkit/internal/media/worker"
	"github.com/open-rails/contentkit/internal/mediamove"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
)

// move copies a pre-v4 bucket (MEDIA_MOVE_SOURCE_BUCKET, on the same
// endpoint and key) into the worker's bucket, which must never have had
// versioning, and records the moved items in the cache. Version items find
// their content in the catalog (MEDIA_CONTENT_SCHEMA), read-only.
func move(ctx context.Context, log *slog.Logger) error {
	source := os.Getenv("MEDIA_MOVE_SOURCE_BUCKET")
	if source == "" {
		return errors.New("MEDIA_MOVE_SOURCE_BUCKET is required")
	}
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
	dest, err := worker.StoreFromEnv()
	if err != nil {
		return err
	}
	from, err := mediaS3.New(dest.Config(source))
	if err != nil {
		return err
	}
	if err := dest.Check(ctx); err != nil {
		return fmt.Errorf("destination bucket: %w", err)
	}
	manifests, err := mediastore.NewManifests(dest, kinds, mediastore.ManifestOptions{})
	if err != nil {
		return err
	}
	versions := pgx.Identifier{cfg.ContentSchema, "content_versions"}.Sanitize()
	report, err := mediamove.Move(ctx, mediamove.Options{Source: from, Dest: dest, Manifests: manifests, Logger: log,
		Contents: func(ctx context.Context, ids []string) (map[string]string, error) {
			rows, err := cfg.Pool.Query(ctx, `SELECT id, content_id FROM `+versions+` WHERE id = ANY($1)`, ids)
			if err != nil {
				return nil, err
			}
			out := map[string]string{}
			for rows.Next() {
				var id, content string
				if err := rows.Scan(&id, &content); err != nil {
					rows.Close()
					return nil, err
				}
				out[id] = content
			}
			rows.Close()
			return out, rows.Err()
		}})
	log.Info("media-worker: move", "items", report.Items, "already", report.Already, "erased", report.Erased,
		"files", report.Files, "bytes", report.Bytes, "dropped", len(report.Dropped))
	for _, d := range report.Dropped {
		log.Warn("media-worker: move dropped", "file", d)
	}
	if err != nil {
		return err
	}
	// The moved items enter the cache, which wakes their releases.
	n, err := worker.Rebuild(ctx, cfg)
	log.Info("media-worker: move recorded", "items", n)
	return err
}
