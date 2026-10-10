//go:build linux || darwin

package video_test

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/open-rails/contentkit/media/internal/s3test"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
)

// A worker killed mid-chunk leaves the job running; River rescues it, the
// rescue costs no attempt, and the outputs equal an uninterrupted encode's.
func TestHardKilledChunkResumesWithIdenticalOutput(t *testing.T) {
	o := opts{chunk: 8 * time.Second}
	if os.Getenv("CONTENTKIT_HARDKILL_CHILD") == "1" {
		var cfg mediaS3.Config
		if err := json.Unmarshal([]byte(os.Getenv("CONTENTKIT_HARDKILL_S3_CONFIG")), &cfg); err != nil {
			t.Fatal(err)
		}
		store, err := mediaS3.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		e := newEnvOn(t, &s3test.Env{Store: store, Config: cfg, Tenant: os.Getenv("CONTENTKIT_HARDKILL_TENANT")}, o,
			os.Getenv("CONTENTKIT_HARDKILL_SCHEMA"), os.Getenv("CONTENTKIT_HARDKILL_CONTENT_SCHEMA"))
		e.start()
		select {}
	}
	e := newEnv(t, o)
	source := fixture{w: 1280, h: 720, secs: 120, rate: 10}.make(t)
	e.start()
	e.put("source", "video/x-matroska", source, nil)
	e.wait()
	baseline := e.manifest()
	e.stopWorker()

	e.ref = e.refOf("video", 2)
	e.put("source", "video/x-matroska", source, nil)
	cfg, err := json.Marshal(e.s3.Config)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestHardKilledChunkResumesWithIdenticalOutput$")
	child.Env = append(os.Environ(), "CONTENTKIT_HARDKILL_CHILD=1", "CONTENTKIT_HARDKILL_SCHEMA="+e.schema,
		"CONTENTKIT_HARDKILL_CONTENT_SCHEMA="+e.contentSchema, "CONTENTKIT_HARDKILL_S3_CONFIG="+string(cfg), "CONTENTKIT_HARDKILL_TENANT="+e.s3.Tenant)
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out strings.Builder
	var outMu sync.Mutex
	child.Stdout, child.Stderr = lockedWriter{&out, &outMu}, lockedWriter{&out, &outMu}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	reaped := false
	defer func() {
		if !reaped {
			_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
			<-exited
		}
	}()
	chunk := e.runningChunk(e.ref, exited, func() string { outMu.Lock(); defer outMu.Unlock(); return out.String() })
	if err := syscall.Kill(-child.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := <-exited; err == nil {
		t.Fatal("hard-killed worker exited successfully")
	}
	reaped = true
	var state string
	if err := e.pool.QueryRow(e.ctx, `SELECT state FROM `+e.schema+`.river_job WHERE id = $1`, chunk).Scan(&state); err != nil || state != "running" {
		t.Fatalf("hard-killed chunk state %q: %v", state, err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE `+e.schema+`.river_job SET attempted_at = now() - interval '3 hours' WHERE id = $1`, chunk); err != nil {
		t.Fatal(err)
	}
	e.start()
	e.wait()
	recovered := e.manifest()
	if r := e.readiness(recovered); !r.Ready() {
		t.Fatalf("readiness %+v", r)
	}
	var attempt int
	var rescued bool
	if err := e.pool.QueryRow(e.ctx, `SELECT attempt, EXISTS (
  SELECT 1 FROM unnest(errors) AS entry WHERE entry->>'error' = 'Stuck job rescued by JobRescuer'
) FROM `+e.schema+`.river_job WHERE id = $1`, chunk).Scan(&attempt, &rescued); err != nil {
		t.Fatal(err)
	}
	if !rescued || attempt != 1 {
		t.Fatalf("hard-killed chunk rescued %v, attempt %d; want a rescue with one working attempt", rescued, attempt)
	}
	if want, got := outputDigests(t, baseline), outputDigests(t, recovered); len(want) == 0 || !maps.Equal(got, want) {
		t.Fatalf("recovered outputs %v, want %v", got, want)
	}
}

type lockedWriter struct {
	b  *strings.Builder
	mu *sync.Mutex
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}
