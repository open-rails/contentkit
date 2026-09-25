package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/tcpproxy"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/media/workqueue"
)

// The worker binary starts with Postgres and the bucket both unreachable:
// it serves its ops port, waits instead of exiting, and reports each
// dependency until it returns.
func TestStartsWithoutItsDependencies(t *testing.T) {
	dsn, endpoint := os.Getenv("CONTENTKIT_TEST_URL"), os.Getenv("CONTENTKIT_TEST_S3_ENDPOINT")
	if dsn == "" || endpoint == "" {
		t.Skip("CONTENTKIT_TEST_URL and CONTENTKIT_TEST_S3_ENDPOINT required")
	}
	ctx := context.Background()
	access, secret := os.Getenv("CONTENTKIT_TEST_S3_ACCESS_KEY"), os.Getenv("CONTENTKIT_TEST_S3_SECRET_KEY")
	bucket := "ck-worker-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	admin, err := mediaS3.New(mediaS3.Config{Bucket: bucket, Endpoint: endpoint, AccessKeyID: access, SecretAccessKey: secret, UsePathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Client().CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { emptyBucket(admin, bucket) })
	schema := "mw_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	t.Cleanup(func() {
		if conn, err := pgx.Connect(ctx, dsn); err == nil {
			_, _ = conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			_ = conn.Close(ctx)
		}
	})

	// The host's migration step, then the worker's own least-privilege login
	// (the production grants): the binary itself runs no DDL.
	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)
	if err := workqueue.Migrate(ctx, adminPool, schema); err != nil {
		t.Fatal(err)
	}
	dsn = pgtest.MediaWorkerRole(t, ctx, adminPool, schema)
	pg, bucketProxy := tcpproxy.New(t, dsn), tcpproxy.New(t, endpoint)
	pg.Down()
	bucketProxy.Down()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	proxiedDSN := strings.Replace(dsn, fmt.Sprintf("%s:%d", cfg.Host, cfg.Port), strings.TrimPrefix(pg.URL, "postgres://"), 1)

	dir := t.TempDir()
	bin := filepath.Join(dir, "media-worker")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	kinds := filepath.Join(dir, "kinds.json")
	if err := os.WriteFile(kinds, []byte(`[{"Name":"clip","Video":{}}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "DATABASE_URL="+proxiedDSN, "MEDIA_WORKER_SCHEMA="+schema, "MEDIA_KINDS_FILE="+kinds,
		"MEDIA_METRICS_ADDR="+addr, "MEDIA_WORKER_TMP="+dir, "MEDIA_S3_ENDPOINT="+bucketProxy.URL, "MEDIA_S3_BUCKET="+bucket,
		"MEDIA_S3_ACCESS_KEY_ID="+access, "MEDIA_S3_SECRET_ACCESS_KEY="+secret)
	cmd.Stderr = testWriter{t}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
		}
	})

	base := "http://" + addr
	status := func() map[string]bool {
		var s struct {
			Dependencies []struct {
				Name string
				Up   bool
			}
		}
		if code, body := get(base + "/statusz"); code == 200 {
			_ = json.Unmarshal([]byte(body), &s)
		}
		out := map[string]bool{}
		for _, d := range s.Dependencies {
			out[d.Name] = d.Up
		}
		return out
	}
	eventually(t, exited, "live while Postgres and the bucket are down", func() bool {
		live, _ := get(base + "/livez")
		ready, _ := get(base + "/readyz")
		s := status()
		up, known := s["postgres"]
		return live == 200 && ready == 503 && known && !up && !s["s3"]
	})
	time.Sleep(2 * time.Second)
	if ready, _ := get(base + "/readyz"); ready != 503 {
		t.Fatalf("ready before Postgres: %d", ready)
	}

	pg.Up(t)
	eventually(t, exited, "ready once Postgres returns, the bucket still down", func() bool {
		ready, _ := get(base + "/readyz")
		_, metrics := get(base + "/metrics")
		s := status()
		return ready == 200 && s["postgres"] && !s["s3"] &&
			strings.Contains(metrics, `app_dependency_up{class="optional",dependency="s3"} 0`)
	})

	bucketProxy.Up(t)
	eventually(t, exited, "the bucket reported up", func() bool {
		_, metrics := get(base + "/metrics")
		return status()["s3"] && strings.Contains(metrics, `app_dependency_up{class="optional",dependency="s3"} 1`)
	})
}

func eventually(t *testing.T, exited <-chan error, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for !cond() {
		select {
		case err := <-exited:
			t.Fatalf("worker exited waiting for %s: %v", what, err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func get(url string) (int, string) {
	resp, err := http.Get(url)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func emptyBucket(s *mediaS3.Store, bucket string) {
	ctx := context.Background()
	for o, err := range s.List(ctx, "") {
		if err != nil {
			break
		}
		_ = s.Delete(ctx, o.Key)
	}
	_, _ = s.Client().DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket})
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}
