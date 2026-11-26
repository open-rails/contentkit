package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/open-rails/helpers/deps"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/open-rails/contentkit/internal/media/image"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/avatar"
)

// Main is the media-worker command, configured by the environment; it exits
// the process. The stock image runs it with no hooks; a host whose kinds'
// recipes name master hooks builds its own worker binary:
//
//	func main() { worker.Main(deskew.Hook{}, upscale.New()) }
//
// Without arguments it runs the worker. The process never exits for a
// missing dependency: it waits for Postgres and the bucket with backoff,
// reporting both on /statusz and app_dependency_up. It runs no DDL: the
// host's contentkit.MigratePostgres migrates MEDIA_WORKER_SCHEMA, so
// DATABASE_URL can be an unprivileged role. A recipe naming a hook the
// binary lacks, or at another version, stops it before any job runs.
//
//	MEDIA_KINDS_FILE    the registry as JSON: Config.MediaRegistry, which `contentkit media-config` prints
//	MEDIA_METRICS_ADDR  ops listen address (default :9090): /metrics, /livez, /readyz, /statusz
//
// and storeFromEnv, tuningFromEnv, DATABASE_URL and DATABASE_SESSION_URL (env.go).
//
// `publish-defaults` renders the default images the kinds' public presets
// name (Public.Default) into each kind's _default item. It needs
// MEDIA_KINDS_FILE and MEDIA_S3_*, no database. MEDIA_DEFAULTS_DIR holds the
// host's own default images, over ContentKit's (media/avatar).
//
// `rebuild` refills the database's item cache from the bucket's manifests
// (the same environment as the worker).
func Main(hooks ...media.MasterHook) {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	var err error
	switch args := os.Args[1:]; strings.Join(args, " ") {
	case "":
		err = serve(ctx, log, hooks)
	case "publish-defaults":
		err = publishDefaults(ctx, os.Stdout)
	case "rebuild":
		err = rebuild(ctx, log)
	default:
		err = fmt.Errorf("unknown command %q: run without arguments, publish-defaults or rebuild", strings.Join(args, " "))
	}
	stop()
	if err != nil {
		log.Error("media-worker", "error", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, log *slog.Logger, hooks []media.MasterHook) error {
	cfg, err := fromEnv(ctx)
	if err != nil {
		return err
	}
	defer cfg.Pool.Close()
	registry := prometheus.NewRegistry()
	cfg.Logger, cfg.Hooks, cfg.Metrics = log, hooks, registry
	sup := deps.New(deps.WithLogger(log))
	sup.AddPostgres("postgres", cfg.Pool.Config().ConnConfig)
	store := cfg.Store
	sup.Add("s3", deps.Optional, func(ctx context.Context) error { return store.Check(ctx) }, nil, deps.ProbeTimeout(5*time.Second))
	sup.Start(ctx)

	addr := os.Getenv("MEDIA_METRICS_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("media-worker: listen for metrics: %w", err)
	}
	if err := registry.Register(depsCollector{sup}); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("GET "+deps.MetricsPath, promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET "+deps.LivePath, sup.Livez)
	mux.HandleFunc("GET "+deps.ReadyPath, sup.Readyz)
	mux.HandleFunc("GET "+deps.StatusPath, sup.Statusz)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	var w *Worker
	var buildErr error
	err = sup.Retry(ctx, "postgres", func(ctx context.Context) error {
		if buildErr = cfg.Pool.Ping(ctx); buildErr == nil {
			w, buildErr = New(ctx, cfg)
		}
		if deps.PostgresUnavailable(buildErr) {
			return buildErr
		}
		return nil
	})
	if err == nil {
		err = buildErr
	}
	if err != nil {
		_ = server.Close()
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	sup.SetReady()

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	workerDone := make(chan error, 1)
	go func() { workerDone <- w.Run(runCtx) }()
	var serveErr error
	select {
	case err = <-workerDone:
	case serveErr = <-serveDone:
		cancelRun()
		err = <-workerDone
	}
	sup.Drain()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	shutdownErr := server.Shutdown(shutdownCtx)
	if serveErr == nil {
		serveErr = <-serveDone
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(err, serveErr, shutdownErr)
}

var (
	dependencyUp = prometheus.NewDesc("app_dependency_up", "1 while the dependency is reachable.", []string{"dependency", "class"}, nil)
	appReady     = prometheus.NewDesc("app_ready", "1 once the worker is built and until it drains.", nil, nil)
)

// depsCollector exports the supervisor's dependency state with the worker's
// Prometheus metrics.
type depsCollector struct{ sup *deps.Supervisor }

func (depsCollector) Describe(ch chan<- *prometheus.Desc) { ch <- dependencyUp; ch <- appReady }

func (c depsCollector) Collect(ch chan<- prometheus.Metric) {
	for _, s := range c.sup.Statuses() {
		ch <- prometheus.MustNewConstMetric(dependencyUp, prometheus.GaugeValue, gauge(s.Up), s.Name, s.Class)
	}
	ch <- prometheus.MustNewConstMetric(appReady, prometheus.GaugeValue, gauge(c.sup.Ready()))
}

func gauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// rebuild refills the item cache from the bucket and logs the count.
func rebuild(ctx context.Context, log *slog.Logger) error {
	cfg, err := fromEnv(ctx)
	if err != nil {
		return err
	}
	defer cfg.Pool.Close()
	cfg.Logger = log
	n, err := Rebuild(ctx, cfg)
	log.Info("media-worker: rebuild", "items", n)
	return err
}

// publishDefaults renders the images the kinds' public presets name
// (Public.Default) into each kind's _default item and its manifest, then
// prints the kinds it wrote. Rendering again changes nothing.
func publishDefaults(ctx context.Context, out io.Writer) error {
	kinds, err := kindsFromEnv()
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
	store, err := storeFromEnv()
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
