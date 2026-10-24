package media

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/open-rails/authkit/auth"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/helpers/eventlog"
)

func TestProgressHTTPReplayIsolationAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var reads, snapshots atomic.Int32
	ref := contentref.ContentRef{ContentKind: "gallery", ContentID: "mine"}
	ended := make(chan struct{})
	log := eventlog.ReaderFunc(func(ctx context.Context, query eventlog.Query) (eventlog.Page, error) {
		n := reads.Add(1)
		if n == 1 && query.After != "resume" {
			t.Errorf("reconnect cursor=%q", query.After)
		}
		fact := ProcessingEvent{Kind: "gallery", ContentID: "other"}
		if n >= 3 {
			fact.ContentID = "mine"
		}
		data, _ := json.Marshal(fact)
		return eventlog.Page{Events: []eventlog.Event{{Data: data}}, NextCursor: "next"}, nil
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(ended)
		_ = serveProgress(w, r, ref, log, r.Header.Get("Last-Event-ID"), func(context.Context) error { return nil }, func(context.Context) (*ReadResult, error) {
			n := snapshots.Add(1)
			return &ReadResult{Files: []FileInfo{}, Status: &MediaStatus{State: StateProcessing, Uploads: ProcessingCount{Completed: int(n), Total: 30}}}, nil
		}, time.Millisecond)
	}))
	defer server.Close()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
	req.Header.Set("Last-Event-ID", "resume")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Type") != "text/event-stream" || res.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("headers=%v", res.Header)
	}
	scanner := bufio.NewScanner(res.Body)
	var frames []string
	var frame strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			frame.WriteString(line + "\n")
			continue
		}
		frames = append(frames, frame.String())
		frame.Reset()
		if len(frames) == 2 {
			break
		}
	}
	if len(frames) != 2 || !strings.Contains(frames[0], "event: snapshot") || !strings.Contains(frames[1], "event: progress") || !strings.Contains(frames[1], `"completed":2`) {
		t.Fatalf("frames=%v err=%v", frames, scanner.Err())
	}
	if snapshots.Load() != 2 || reads.Load() < 3 {
		t.Fatalf("another owner's event caused a snapshot: snapshots=%d pages=%d", snapshots.Load(), reads.Load())
	}
	cancel()
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("disconnect leaked stream")
	}
}

func TestProgressHTTPRetentionGapAndRevocation(t *testing.T) {
	var calls atomic.Int32
	var valid atomic.Bool
	valid.Store(true)
	ended := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(ended)
		log := eventlog.ReaderFunc(func(_ context.Context, q eventlog.Query) (eventlog.Page, error) {
			calls.Add(1)
			if q.After == "expired" {
				return eventlog.Page{}, eventlog.ErrExpiredCursor
			}
			return eventlog.Page{NextCursor: "new"}, nil
		})
		_ = serveProgress(w, r, contentref.ContentRef{}, log, "expired", func(context.Context) error {
			if !valid.Load() {
				return auth.ErrRevoked
			}
			return nil
		}, func(context.Context) (*ReadResult, error) {
			return &ReadResult{Files: []FileInfo{}, Status: &MediaStatus{State: StateProcessing}}, nil
		}, time.Millisecond)
	}))
	defer server.Close()
	res, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	scanner := bufio.NewScanner(res.Body)
	var first string
	for scanner.Scan() {
		first += scanner.Text() + "\n"
		if scanner.Text() == "" {
			break
		}
	}
	if !strings.Contains(first, "event: resync") || !strings.Contains(first, "id: new") {
		t.Fatalf("gap not explicit: %s", first)
	}
	valid.Store(false)
	var tail strings.Builder
	for scanner.Scan() {
		tail.WriteString(scanner.Text() + "\n")
	}
	if !strings.Contains(tail.String(), "event: error") || !strings.Contains(tail.String(), "unauthorized") {
		t.Fatalf("revocation=%s", tail.String())
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("revocation leaked stream")
	}
	if calls.Load() < 2 {
		t.Fatal("did not recover cursor")
	}
}

func TestProgressHTTPInvalidCursorDoesNotReadSnapshot(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/?status=1", nil)
	err := serveProgress(recorder, request, contentref.ContentRef{}, eventlog.ReaderFunc(func(context.Context, eventlog.Query) (eventlog.Page, error) {
		return eventlog.Page{}, eventlog.ErrInvalidCursor
	}), "another-scope", func(context.Context) error { t.Fatal("invalid cursor reached authorization"); return nil }, func(context.Context) (*ReadResult, error) {
		t.Fatal("invalid cursor read a snapshot")
		return nil, nil
	}, time.Millisecond)
	if err != eventlog.ErrInvalidCursor || recorder.Code != http.StatusBadRequest || strings.Contains(recorder.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("invalid cursor: status=%d error=%v", recorder.Code, err)
	}
}
