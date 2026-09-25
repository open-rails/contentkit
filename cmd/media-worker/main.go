// Command media-worker is the stock media worker (media/worker) for hosts
// whose kinds are plain data: it reads them from a JSON file. Hosts with a
// per-file spec chooser or hooks (Failed, SlotEncoded) build their own worker
// from the code that builds their media.Registry instead.
//
// Environment: worker.FromEnv and Config.TuningFromEnv, plus
//
//	MEDIA_KINDS_FILE    JSON array of media.Kind, the host's registry (e.g. [{"Name":"clip","Video":{}}])
//	MEDIA_METRICS_ADDR  ops listen address (default :9090): /metrics, /livez, /readyz, /statusz
//
// The process never exits for a missing dependency: it waits for Postgres and
// the bucket with backoff, reporting both on /statusz and app_dependency_up.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/open-rails/helpers/deps"

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
	registry := prometheus.NewRegistry()
	cfg.Metrics, err = worker.NewMetrics(registry)
	if err != nil {
		return err
	}
	sup := deps.New(deps.WithLogger(log))
	sup.AddPostgres("postgres", cfg.Pool.Config().ConnConfig)
	store := cfg.Store
	sup.Add("s3", deps.Optional, func(ctx context.Context) error { return store.Check(ctx, "_media-worker/") }, nil, deps.ProbeTimeout(5*time.Second))
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

	var w *worker.Worker
	var buildErr error
	err = sup.Retry(ctx, "postgres", func(ctx context.Context) error {
		w, buildErr = worker.New(ctx, cfg)
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
