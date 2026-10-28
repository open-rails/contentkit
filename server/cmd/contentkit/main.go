// Command contentkit is ContentKit as its own server (package server): the
// engine's routes behind an AuthKit resource server, entitlements from
// OpenRails, health at /livez, /readyz and /statusz.
//
//	contentkit -config contentkit.yaml               serve
//	contentkit media-config -config contentkit.yaml  print the media worker's registry
//	contentkit reconcile -config contentkit.yaml     repair derived catalog state
//
// The config file is server.Config as YAML. media-config prints the engine's
// media registry (contentkit.Config.MediaRegistry) as the JSON cmd/media-worker
// reads from MEDIA_KINDS_FILE. The environment holds the connections and
// secrets:
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
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/contentkit/server"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log, os.Args[1:], os.Stdout); err != nil {
		log.Error("contentkit", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, args []string, stdout io.Writer) error {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	flags := flag.NewFlagSet("contentkit "+cmd, flag.ContinueOnError)
	path := flags.String("config", "contentkit.yaml", "config file (server.Config as YAML)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	raw, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	cfg, err := server.Load(raw, os.Getenv)
	if err != nil {
		return err
	}
	switch cmd {
	case "serve":
		return serve(log, cfg, nil)
	case "media-config":
		return mediaConfig(cfg, stdout)
	case "reconcile":
		return serve(log, cfg, func(ctx context.Context, srv *server.Server) error {
			n, err := srv.Reconcile(ctx)
			if err == nil {
				_, err = fmt.Fprintf(stdout, "reconciled facets of %d and paths of %d content items\n", n.Facets, n.Paths)
			}
			return err
		})
	}
	return fmt.Errorf("unknown command %q: serve, media-config or reconcile", cmd)
}

// mediaConfig writes the engine's media registry as cmd/media-worker reads it.
func mediaConfig(cfg server.Config, w io.Writer) error {
	b, err := cfg.Engine.MediaRegistry()
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// serve runs the server until a signal, or, with task, builds it and runs task instead.
func serve(log *slog.Logger, cfg server.Config, task func(context.Context, *server.Server) error) error {
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
	if task != nil {
		return errors.Join(task(ctx, srv), srv.Close(context.WithoutCancel(ctx)))
	}
	return srv.Run(ctx)
}
