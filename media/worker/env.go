package worker

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/media"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
)

// FromEnv builds a Config's database and bucket from the environment, with
// TuningFromEnv; the host then sets Kinds, Specs and Hooks. Close the Pool
// when done.
//
//	DATABASE_URL                 host Postgres (holds workqueue.Schema)
//	MEDIA_S3_ENDPOINT            e.g. http://rook-ceph-rgw-external.svc
//	MEDIA_S3_BUCKET, MEDIA_S3_REGION (default us-east-1), MEDIA_S3_PATH_STYLE (default true)
//	MEDIA_S3_ACCESS_KEY_ID, MEDIA_S3_SECRET_ACCESS_KEY   read/write key
func FromEnv(ctx context.Context) (Config, error) {
	var c Config
	if err := c.TuningFromEnv(); err != nil {
		return c, err
	}
	pathStyle, err := strconv.ParseBool(envOr("MEDIA_S3_PATH_STYLE", "true"))
	if err != nil {
		return c, fmt.Errorf("MEDIA_S3_PATH_STYLE: %w", err)
	}
	s3 := mediaS3.Config{
		Bucket:          os.Getenv("MEDIA_S3_BUCKET"),
		Region:          os.Getenv("MEDIA_S3_REGION"),
		Endpoint:        os.Getenv("MEDIA_S3_ENDPOINT"),
		AccessKeyID:     os.Getenv("MEDIA_S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("MEDIA_S3_SECRET_ACCESS_KEY"),
		UsePathStyle:    pathStyle,
	}
	probe, err := mediaS3.New(s3)
	if err != nil {
		return c, err
	}
	if s3.Capabilities, err = media.Probe(ctx, probe, "_media-worker/"); err != nil {
		return c, err
	}
	if c.Store, err = mediaS3.New(s3); err != nil {
		return c, err
	}
	if c.Pool, err = pgxpool.New(ctx, os.Getenv("DATABASE_URL")); err != nil {
		return c, fmt.Errorf("DATABASE_URL: %w", err)
	}
	return c, nil
}

// TuningFromEnv sets the host queue and tuning fields present in the
// environment, leaving the others as they are:
//
//	MEDIA_HOST_RIVER_SCHEMA      the host's River schema (default: the connection's search path)
//	MEDIA_HOST_QUEUE             the host's media queue (default contentkit_media)
//	MEDIA_HOST_GRACE             the host's sweep grace (default 24h)
//	MEDIA_WORKER_TMP             scratch dir (default os.TempDir()); size for a video source plus outputs
//	MEDIA_WORKER_THREADS         ffmpeg threads (default: CPU limit)
//	MEDIA_WORKER_PRESET          x264 preset of rungs up to 1080 (default faster)
//	MEDIA_WORKER_TOP_PRESET      x264 preset of 1440/2160 (default faster)
//	MEDIA_WORKER_ENCODER         auto (default: NVENC if a probe encode works, else x264), x264 or nvenc
//	MEDIA_WORKER_CONCURRENCY     video jobs per process (default 1)
//	MEDIA_WORKER_IMAGE_CONCURRENCY  image jobs per process (default 2)
//	MEDIA_WORKER_JOB_TIMEOUT     per video job (default 48h)
//	MEDIA_WORKER_SHUTDOWN_GRACE  time running jobs get on SIGTERM before cancel (default 30s)
func (c *Config) TuningFromEnv() error {
	for k, p := range map[string]*string{"MEDIA_HOST_RIVER_SCHEMA": &c.HostSchema, "MEDIA_HOST_QUEUE": &c.HostQueue, "MEDIA_WORKER_TMP": &c.TempDir,
		"MEDIA_WORKER_PRESET": &c.Preset, "MEDIA_WORKER_TOP_PRESET": &c.TopPreset, "MEDIA_WORKER_ENCODER": &c.VideoEncoder} {
		if v := os.Getenv(k); v != "" {
			*p = v
		}
	}
	for k, p := range map[string]*int{"MEDIA_WORKER_THREADS": &c.Threads, "MEDIA_WORKER_CONCURRENCY": &c.VideoWorkers,
		"MEDIA_WORKER_IMAGE_CONCURRENCY": &c.ImageWorkers} {
		n, err := intEnv(k, *p)
		if err != nil {
			return err
		}
		*p = n
	}
	for k, p := range map[string]*time.Duration{"MEDIA_HOST_GRACE": &c.Grace, "MEDIA_WORKER_JOB_TIMEOUT": &c.VideoTimeout,
		"MEDIA_WORKER_SHUTDOWN_GRACE": &c.ShutdownGrace} {
		d, err := durationEnv(k, *p)
		if err != nil {
			return err
		}
		*p = d
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
