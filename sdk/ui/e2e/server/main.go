// Command server runs a real ContentKit for @openrails/contentkit-ui's
// integration and browser tests: AuthKit for identity (adapters/authkit, as
// hosts wire it), the ContentKit Runtime (content, taxonomy, codes, media
// upload and read) over PostgreSQL and MinIO, its River jobs, the built demo
// apps and the bucket on one origin, a fault proxy in front of media-gateway
// on another, and a test-control surface at /__test. Test-only.
//
// e2e/support/stack.ts starts it with the compose services
// (e2e/compose.yaml); it prints "READY" once serving and runs until SIGTERM,
// stdin's end (-stdin), its parent's exit, -idle without requests, -lifetime
// or Postgres down, then runs -teardown (the stack's `down`).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type config struct {
	addr, origin       string // the app, API, bucket and /__test origin
	mediaAddr          string // the fault proxy; its origin is the registry's BaseURL
	gateway            string // media-gateway behind the fault proxy
	dsn                string
	s3Endpoint, bucket string
	s3Key, s3Secret    string
	tokenKey           string // media-gateway's MEDIA_GATEWAY_TOKEN_KEY
	kinds, static      string
	lifetime, idle     time.Duration
	stdin              bool
	teardown           []string
}

func main() {
	var c config
	flag.StringVar(&c.addr, "addr", "127.0.0.1:4790", "listen address of the app origin")
	flag.StringVar(&c.origin, "origin", "", "public app origin (default http://localhost:<port>)")
	flag.StringVar(&c.mediaAddr, "media-addr", "127.0.0.1:4791", "listen address of the fault proxy (the media origin)")
	flag.StringVar(&c.gateway, "gateway", "", "media-gateway URL")
	flag.StringVar(&c.dsn, "dsn", os.Getenv("DATABASE_URL"), "PostgreSQL DSN")
	flag.StringVar(&c.s3Endpoint, "s3-endpoint", "", "MinIO endpoint")
	flag.StringVar(&c.bucket, "bucket", "ckui-e2e", "bucket, created if missing")
	flag.StringVar(&c.s3Key, "s3-access-key", "contentkit", "S3 access key")
	flag.StringVar(&c.s3Secret, "s3-secret-key", "contentkit-secret", "S3 secret key")
	flag.StringVar(&c.tokenKey, "token-key", "", "media token key {kid}:{base64 secret}")
	flag.StringVar(&c.kinds, "kinds", "kinds.json", "media registry JSON, shared with the worker")
	flag.StringVar(&c.static, "static", "", "directory served at /")
	flag.DurationVar(&c.lifetime, "lifetime", time.Hour, "exit after this long")
	flag.DurationVar(&c.idle, "idle", 10*time.Minute, "exit after this long without a request; 0 never")
	flag.BoolVar(&c.stdin, "stdin", false, "exit when stdin closes")
	flag.Func("teardown", "JSON argv run at exit, e.g. the compose stack's down", func(v string) error { return json.Unmarshal([]byte(v), &c.teardown) })
	flag.Parse()
	if c.origin == "" {
		_, port, err := net.SplitHostPort(c.addr)
		if err != nil {
			log.Fatal(err)
		}
		c.origin = "http://localhost:" + port
	}
	err := run(c)
	teardown(c.teardown)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e server:", err)
		os.Exit(1)
	}
}

func run(c config) error {
	if c.dsn == "" || c.s3Endpoint == "" || c.gateway == "" || c.tokenKey == "" {
		return errors.New("-dsn, -s3-endpoint, -gateway and -token-key are required")
	}
	out := &cappedWriter{w: os.Stderr, limit: 16 << 20}
	slog.SetDefault(slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelWarn})))
	log.SetOutput(out)
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	l := newLifecycle(signals)
	l.watch(c.stdin, c.lifetime, c.idle)
	ctx := l.ctx

	_, mediaPort, err := net.SplitHostPort(c.mediaAddr)
	if err != nil {
		return err
	}
	// Another origin on the same site, as media.example.com is to example.com:
	// the gateway's Cross-Origin-Resource-Policy is same-site.
	mediaOrigin := "http://localhost:" + mediaPort
	h, err := newHarness(ctx, c, mediaOrigin)
	if err != nil {
		return err
	}
	l.watchPostgres(h.pool, 20*time.Second)

	gateway, err := url.Parse(c.gateway)
	if err != nil {
		return err
	}
	appSrv := &http.Server{Addr: c.addr, Handler: l.active(h.routes(c.static)), ReadHeaderTimeout: 10 * time.Second}
	mediaSrv := &http.Server{Addr: c.mediaAddr, Handler: l.active(h.faults.gatewayProxy(gateway)), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2)
	for _, s := range []*http.Server{appSrv, mediaSrv} {
		ln, err := net.Listen("tcp", s.Addr)
		if err != nil {
			return err
		}
		go func() { errc <- s.Serve(ln) }()
	}
	fmt.Printf("READY %s %s\n", c.origin, mediaOrigin)
	var failed error
	select {
	case failed = <-errc:
	case <-ctx.Done():
		if cause := context.Cause(ctx); errors.Is(cause, errPostgres) {
			failed = cause
		} else {
			log.Printf("e2e server: stopping: %v", cause)
		}
	}
	l.cancel(errors.New("stopping"))
	// Stopping is bounded: a stuck dependency never keeps the process alive.
	done := make(chan struct{})
	go func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = errors.Join(appSrv.Shutdown(shutdown), mediaSrv.Shutdown(shutdown))
		h.close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		log.Print("e2e server: shutdown timed out")
	}
	return failed
}

// clientIP is the request's peer address.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}
