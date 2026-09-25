// Command media-worker is the stock media worker (media/worker) for hosts
// whose kinds are plain data: it reads them from a JSON file. Hosts with a
// per-file spec chooser or hooks (Failed, SlotEncoded) build their own worker
// from the code that builds their media.Registry instead.
//
// Environment: worker.FromEnv and Config.TuningFromEnv, plus
//
//	MEDIA_KINDS_FILE    JSON array of media.Kind, the host's registry (e.g. [{"Name":"clip","Video":{}}])
//	MEDIA_METRICS_ADDR  metrics listen address (default :9090)
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

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/worker"
	queuemetrics "github.com/open-rails/contentkit/media/workqueue/metrics"
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
	queueCollector, err := queuemetrics.NewCollector(cfg.Pool, cfg.Schema)
	if err != nil {
		return err
	}
	if err := registry.Register(queueCollector); err != nil {
		return err
	}
	cfg.RiverHooks = append(cfg.RiverHooks, queueCollector.LeaderHook())
	w, err := worker.New(ctx, cfg)
	if err != nil {
		return err
	}
	addr := os.Getenv("MEDIA_METRICS_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("media-worker: listen for metrics: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
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
