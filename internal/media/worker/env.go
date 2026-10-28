package worker

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/media"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
)

// FromEnv builds a Config's database and bucket (StoreFromEnv) from the
// environment, with TuningFromEnv; the host then sets Kinds. Neither is
// dialed. Close the Pool when done.
//
//	DATABASE_URL                 host Postgres (holds MEDIA_WORKER_SCHEMA)
func FromEnv(ctx context.Context) (Config, error) {
	var c Config
	if err := c.TuningFromEnv(); err != nil {
		return c, err
	}
	store, err := StoreFromEnv()
	if err != nil {
		return c, err
	}
	c.Store = store
	if c.Pool, err = pgxpool.New(ctx, os.Getenv("DATABASE_URL")); err != nil {
		return c, fmt.Errorf("DATABASE_URL: %w", err)
	}
	return c, nil
}

// StoreFromEnv is the media bucket, not dialed:
//
//	MEDIA_S3_ENDPOINT            e.g. http://rook-ceph-rgw-external.svc
//	MEDIA_S3_BUCKET, MEDIA_S3_REGION (default us-east-1), MEDIA_S3_PATH_STYLE (default true)
//	MEDIA_S3_ACCESS_KEY_ID, MEDIA_S3_SECRET_ACCESS_KEY   read/write key
func StoreFromEnv() (*mediaS3.Store, error) {
	pathStyle, err := strconv.ParseBool(envOr("MEDIA_S3_PATH_STYLE", "true"))
	if err != nil {
		return nil, fmt.Errorf("MEDIA_S3_PATH_STYLE: %w", err)
	}
	return mediaS3.New(mediaS3.Config{
		Bucket:          os.Getenv("MEDIA_S3_BUCKET"),
		Region:          os.Getenv("MEDIA_S3_REGION"),
		Endpoint:        os.Getenv("MEDIA_S3_ENDPOINT"),
		AccessKeyID:     os.Getenv("MEDIA_S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("MEDIA_S3_SECRET_ACCESS_KEY"),
		UsePathStyle:    pathStyle,
	})
}

// TuningFromEnv sets the host queue and tuning fields present in the
// environment, leaving the others as they are:
//
//	MEDIA_WORKER_SCHEMA          the host's worker River schema (required, one per host, e.g. doujins_media_worker)
//	MEDIA_CONTENT_SCHEMA         the shared ContentKit schema (required; same as the uploader's journal)
//	MEDIA_HOST_RIVER_SCHEMA      the host's River schema (default: the connection's search path)
//	MEDIA_HOST_GRACE             the host's sweep grace (default 24h)
//	MEDIA_WORKER_TMP             scratch dir (default os.TempDir()); size for a video source plus outputs
//	MEDIA_WORKER_THREADS         ffmpeg threads (default: CPU limit)
//	MEDIA_WORKER_PRESET          x264 preset of rungs up to 1080 (default fast)
//	MEDIA_WORKER_TOP_PRESET      x264 preset of the rungs above (default fast)
//	MEDIA_WORKER_CODECS          the ladder's codecs, preferred first: h264, av1 (default av1,h264)
//	MEDIA_WORKER_ENCODER         auto (default: per codec NVENC if a probe encode works, else CPU), cpu or nvenc
//	MEDIA_WORKER_CONCURRENCY     image/video derivative jobs per process (default 1)
//	MEDIA_WORKER_INGEST_CONCURRENCY  ingest jobs per process (default 2)
//	MEDIA_WORKER_SHUTDOWN_GRACE  time running jobs get on SIGTERM before cancel (default 30s)
func (c *Config) TuningFromEnv() error {
	for k, p := range map[string]*string{"MEDIA_ACCOUNT_SCHEMA": &c.AccountSchema, "MEDIA_ACCOUNT_WORKER_SCHEMA": &c.AccountWorkerSchema, "MEDIA_WORKER_SCHEMA": &c.Schema, "MEDIA_CONTENT_SCHEMA": &c.ContentSchema, "MEDIA_HOST_RIVER_SCHEMA": &c.HostSchema, "MEDIA_WORKER_TMP": &c.TempDir,
		"MEDIA_WORKER_PRESET": &c.Preset, "MEDIA_WORKER_TOP_PRESET": &c.TopPreset, "MEDIA_WORKER_ENCODER": &c.VideoEncoder} {
		if v := os.Getenv(k); v != "" {
			*p = v
		}
	}
	if v := os.Getenv("MEDIA_WORKER_CODECS"); v != "" {
		c.VideoCodecs = nil
		for _, s := range strings.Split(v, ",") {
			c.VideoCodecs = append(c.VideoCodecs, media.Codec(strings.TrimSpace(s)))
		}
	}
	for k, p := range map[string]*int{"MEDIA_WORKER_THREADS": &c.Threads, "MEDIA_WORKER_CONCURRENCY": &c.DerivativeWorkers,
		"MEDIA_WORKER_INGEST_CONCURRENCY": &c.IngestWorkers} {
		n, err := intEnv(k, *p)
		if err != nil {
			return err
		}
		*p = n
	}
	for k, p := range map[string]*time.Duration{"MEDIA_HOST_GRACE": &c.Grace,
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
