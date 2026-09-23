// Command media-access is the media access worker. Settings are flags or
// their environment variables; secrets are environment only, each also
// readable from a file named by {VAR}_FILE:
//
//	-listen        MEDIA_ACCESS_LISTEN          default :8080
//	-s3-endpoint   MEDIA_ACCESS_S3_ENDPOINT     path-style S3 endpoint (RGW or MinIO)
//	-s3-bucket     MEDIA_ACCESS_S3_BUCKET
//	-s3-region     MEDIA_ACCESS_S3_REGION       default us-east-1
//	-hosts         MEDIA_ACCESS_HOSTS           comma list of allowed Host names; empty allows any
//	-cors-origins  MEDIA_ACCESS_CORS_ORIGINS    comma list of exact origins allowed with credentials
//	               MEDIA_ACCESS_S3_ACCESS_KEY_ID, MEDIA_ACCESS_S3_SECRET_ACCESS_KEY   read-only key
//	               MEDIA_ACCESS_TOKEN_KEY           current signing key "{kid}:{base64 secret}"
//	               MEDIA_ACCESS_TOKEN_KEY_PREVIOUS  previous key, accepted during rotation; optional
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/open-rails/contentkit/media/accessworker"
	"github.com/open-rails/contentkit/media/token"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log, os.Args[1:]); err != nil {
		log.Error("media-access", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("media-access", flag.ContinueOnError)
	listen := fs.String("listen", env("MEDIA_ACCESS_LISTEN", ":8080"), "listen address")
	endpoint := fs.String("s3-endpoint", env("MEDIA_ACCESS_S3_ENDPOINT", ""), "S3 endpoint")
	bucket := fs.String("s3-bucket", env("MEDIA_ACCESS_S3_BUCKET", ""), "bucket")
	region := fs.String("s3-region", env("MEDIA_ACCESS_S3_REGION", "us-east-1"), "S3 region")
	hosts := fs.String("hosts", env("MEDIA_ACCESS_HOSTS", ""), "allowed Host names")
	origins := fs.String("cors-origins", env("MEDIA_ACCESS_CORS_ORIGINS", ""), "CORS origins")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var errs []error
	secret := func(name string, required bool) string {
		v, err := secretEnv(name)
		if err == nil && v == "" && required {
			err = fmt.Errorf("%s is required", name)
		}
		errs = append(errs, err)
		return v
	}
	accessKey := secret("MEDIA_ACCESS_S3_ACCESS_KEY_ID", true)
	secretKey := secret("MEDIA_ACCESS_S3_SECRET_ACCESS_KEY", true)
	current := secret("MEDIA_ACCESS_TOKEN_KEY", true)
	previous := secret("MEDIA_ACCESS_TOKEN_KEY_PREVIOUS", false)
	if err := errors.Join(errs...); err != nil {
		return err
	}
	ring, err := token.ParseRing(current, previous)
	if err != nil {
		return err
	}
	h, err := accessworker.New(accessworker.Config{
		Endpoint: *endpoint, Bucket: *bucket, Region: *region,
		AccessKeyID: accessKey, SecretAccessKey: secretKey,
		Ring: ring, Hosts: list(*hosts), Origins: list(*origins), Logger: log,
	})
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	log.Info("listening", "addr", ln.Addr().String(), "bucket", *bucket)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

// secretEnv reads name, or the file named by name_FILE.
func secretEnv(name string) (string, error) {
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	path := os.Getenv(name + "_FILE")
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s_FILE: %w", name, err)
	}
	return strings.TrimSpace(string(b)), nil
}

func list(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
