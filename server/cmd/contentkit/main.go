// Command contentkit is ContentKit as its own server (package server): the
// engine's routes behind an AuthKit resource server, entitlements from
// OpenRails, health at /livez, /readyz and /statusz.
//
//	contentkit -config contentkit.yaml
//
// The config file is server.Config as YAML. The environment holds the
// connections and secrets:
//
//	DATABASE_URL  Postgres (required): ContentKit's, River's and AuthKit's schemas
//	REDIS_URL     optional: shared rate limits
//	CONTENTKIT_MEDIA_TOKEN_KEY, CONTENTKIT_MEDIA_TOKEN_PREVIOUS_KEY,
//	CONTENTKIT_S3_ACCESS_KEY_ID, CONTENTKIT_S3_SECRET_ACCESS_KEY,
//	CONTENTKIT_CLICKHOUSE_PASSWORD, CONTENTKIT_OPENRAILS_API_KEY
//
// SIGTERM drains: /readyz fails for drain_delay, then in-flight requests
// finish and the jobs stop.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/contentkit/server"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("contentkit", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	path := flag.String("config", "contentkit.yaml", "config file (server.Config as YAML)")
	flag.Parse()
	raw, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	cfg, err := server.Load(raw, os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	d := server.Deps{Postgres: pool, Logger: log}
	if u := os.Getenv("REDIS_URL"); u != "" {
		opts, err := redis.ParseURL(u)
		if err != nil {
			return err
		}
		rdb := redis.NewClient(opts)
		defer rdb.Close()
		d.Redis = rdb
	}
	srv, err := server.New(ctx, cfg, d)
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}
