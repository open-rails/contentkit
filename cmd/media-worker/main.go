// Command media-worker runs media/video encode jobs from River schema
// media_worker in the host database.
//
// Environment:
//
//	DATABASE_URL                 host Postgres (holds media_worker)
//	MEDIA_S3_ENDPOINT            RGW endpoint, e.g. http://rook-ceph-rgw-external.svc
//	MEDIA_S3_BUCKET, MEDIA_S3_REGION (default us-east-1), MEDIA_S3_PATH_STYLE (default true)
//	MEDIA_S3_ACCESS_KEY_ID, MEDIA_S3_SECRET_ACCESS_KEY   host read/write key
//	MEDIA_WORKER_TMP             scratch dir (default os.TempDir()); size for source + outputs
//	MEDIA_WORKER_THREADS         ffmpeg threads (default: CPU limit)
//	MEDIA_WORKER_CONCURRENCY     jobs per process (default 1)
//	MEDIA_WORKER_JOB_TIMEOUT     per-job limit (default 48h)
//	MEDIA_WORKER_SHUTDOWN_GRACE  time running jobs get to finish on SIGTERM before cancel (default 30s)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"

	"github.com/open-rails/contentkit/media"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/media/video"
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

	threads, err := intEnv("MEDIA_WORKER_THREADS", 0)
	if err != nil {
		return err
	}
	concurrency, err := intEnv("MEDIA_WORKER_CONCURRENCY", 1)
	if err != nil {
		return err
	}
	timeout, err := durationEnv("MEDIA_WORKER_JOB_TIMEOUT", 48*time.Hour)
	if err != nil {
		return err
	}
	grace, err := durationEnv("MEDIA_WORKER_SHUTDOWN_GRACE", 30*time.Second)
	if err != nil {
		return err
	}
	pathStyle, err := strconv.ParseBool(envOr("MEDIA_S3_PATH_STYLE", "true"))
	if err != nil {
		return fmt.Errorf("MEDIA_S3_PATH_STYLE: %w", err)
	}
	tmp := os.Getenv("MEDIA_WORKER_TMP")

	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("DATABASE_URL: %w", err)
	}
	defer pool.Close()
	if err := video.Migrate(ctx, pool); err != nil {
		return err
	}

	cfg := mediaS3.Config{
		Bucket:          os.Getenv("MEDIA_S3_BUCKET"),
		Region:          os.Getenv("MEDIA_S3_REGION"),
		Endpoint:        os.Getenv("MEDIA_S3_ENDPOINT"),
		AccessKeyID:     os.Getenv("MEDIA_S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("MEDIA_S3_SECRET_ACCESS_KEY"),
		UsePathStyle:    pathStyle,
	}
	probeStore, err := mediaS3.New(cfg)
	if err != nil {
		return err
	}
	if cfg.Capabilities, err = media.Probe(ctx, probeStore, "_media-worker/"); err != nil {
		return err
	}
	store, err := mediaS3.New(cfg)
	if err != nil {
		return err
	}
	log.Info("media-worker: bucket", "bucket", cfg.Bucket, "capabilities", cfg.Capabilities)

	if err := video.SweepTemp(tmp); err != nil {
		return fmt.Errorf("sweep scratch: %w", err)
	}
	enc, err := video.New(video.Config{Store: store, Locker: media.PGLocker(pool), TempDir: tmp, Threads: threads, Logger: log})
	if err != nil {
		return err
	}
	wc := video.WorkerConfig{Encoder: enc, Pool: pool, Timeout: timeout, MaxWorkers: concurrency, Logger: log}
	jobs, err := video.Contribution(wc)
	if err != nil {
		return err
	}
	client, err := riverhelpers.New(ctx, pool, video.ClientConfig(wc), jobs)
	if err != nil {
		return err
	}
	if err := client.Start(ctx); err != nil {
		return err
	}
	log.Info("media-worker: started", "schema", video.Schema, "queue", video.Queue, "concurrency", concurrency, "timeout", timeout)
	<-ctx.Done()

	// Finish within grace if possible; otherwise cancel (ffmpeg is killed,
	// scratch removed) and the job retries from scratch elsewhere.
	soft, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := client.Stop(soft); err == nil {
		return nil
	}
	hard, cancelHard := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelHard()
	if err := client.StopAndCancel(hard); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func intEnv(k string, def int) (int, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: invalid %q", k, v)
	}
	return n, nil
}

func durationEnv(k string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: invalid %q", k, v)
	}
	return d, nil
}
