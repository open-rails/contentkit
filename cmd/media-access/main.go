// Command media-access is the media access agent (media/agent). Settings are
// flags or their environment variables; secrets are environment only, each
// also readable from a file named by {VAR}_FILE:
//
//	-listen        MEDIA_ACCESS_LISTEN          default :8080
//	-s3-endpoint   MEDIA_ACCESS_S3_ENDPOINT     path-style S3 endpoint (RGW or MinIO)
//	-s3-bucket     MEDIA_ACCESS_S3_BUCKET
//	-s3-region     MEDIA_ACCESS_S3_REGION       default us-east-1
//	-hosts         MEDIA_ACCESS_HOSTS           required: each media host and the namespaces it serves,
//	                                            e.g. "media.doujins.ai=doujins,accounts; media.hanime.media=hentai0,accounts"
//	-cors-origins  MEDIA_ACCESS_CORS_ORIGINS    comma list of exact site origins allowed with credentials,
//	                                            e.g. https://doujins.ai; empty breaks hls.js (warned)
//	-defaults      MEDIA_ACCESS_DEFAULTS        public names that fall back to the kind's _default item,
//	                                            e.g. "doujins/gallery: cover-{w}.webp; accounts/user: avatar-{w}.webp"
//	               MEDIA_ACCESS_S3_ACCESS_KEY_ID, MEDIA_ACCESS_S3_SECRET_ACCESS_KEY   key reading only */private/* and */public/*
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

	"github.com/open-rails/contentkit/media/agent"
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
	hosts := fs.String("hosts", env("MEDIA_ACCESS_HOSTS", ""), "host=namespace,…; …")
	origins := fs.String("cors-origins", env("MEDIA_ACCESS_CORS_ORIGINS", ""), "CORS origins")
	defaults := fs.String("defaults", env("MEDIA_ACCESS_DEFAULTS", ""), "namespace/kind: name template,…; …")
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
	hostMap, err := agent.ParseHosts(*hosts)
	if err != nil {
		return fmt.Errorf("MEDIA_ACCESS_HOSTS: %w", err)
	}
	defs, err := agent.ParseDefaults(*defaults)
	if err != nil {
		return fmt.Errorf("MEDIA_ACCESS_DEFAULTS: %w", err)
	}
	h, err := agent.New(agent.Config{
		Endpoint: *endpoint, Bucket: *bucket, Region: *region,
		AccessKeyID: accessKey, SecretAccessKey: secretKey,
		Ring: ring, Hosts: hostMap, Origins: list(*origins), Defaults: defs, Logger: log,
	})
	if err != nil {
		return err
	}
	if len(list(*origins)) == 0 {
		log.Warn("MEDIA_ACCESS_CORS_ORIGINS is empty: browsers cannot read blobs with fetch/XHR, so hls.js playback fails; set it to the sites' exact origins")
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
	log.Info("listening", "addr", ln.Addr().String(), "bucket", *bucket, "hosts", agent.FormatHosts(hostMap), "defaults", agent.FormatDefaults(defs))
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
