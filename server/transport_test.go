package server

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The body deadline, through a real connection: a stalled body is answered
// once it passes, a body read to its end lifts it for a long answer, and a
// request without a body never gets one.
func TestBoundReads(t *testing.T) {
	const d = 300 * time.Millisecond
	srv := httptest.NewServer(boundReads(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "body", http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/slow" {
			_, _ = io.WriteString(w, "started\n")
			_ = http.NewResponseController(w).Flush()
			time.Sleep(3 * d)
			if r.Context().Err() != nil {
				t.Errorf("%s %s: the deadline cancelled the answer: %v", r.Method, r.URL.Path, r.Context().Err())
			}
		}
		_, _ = io.WriteString(w, "done\n")
	}), d))
	defer srv.Close()
	exchange := func(t *testing.T, request string) (*http.Response, time.Duration) {
		t.Helper()
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		start := time.Now()
		if _, err := io.WriteString(conn, request); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(10 * d))
		res, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("no answer: %v", err)
		}
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("answer cut: %v (%q)", err, body)
		}
		return res, time.Since(start)
	}
	t.Run("a stalled body is answered at the deadline", func(t *testing.T) {
		res, took := exchange(t, "POST /stall HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n{")
		if res.StatusCode != http.StatusBadRequest || took < d || took > 5*d {
			t.Fatalf("%d after %s", res.StatusCode, took)
		}
	})
	t.Run("a read body lifts the deadline", func(t *testing.T) {
		res, took := exchange(t, "POST /slow HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")
		if res.StatusCode != http.StatusOK || took < 3*d {
			t.Fatalf("%d after %s", res.StatusCode, took)
		}
	})
	t.Run("no body, no deadline", func(t *testing.T) {
		res, took := exchange(t, "GET /slow HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
		if res.StatusCode != http.StatusOK || took < 3*d {
			t.Fatalf("%d after %s", res.StatusCode, took)
		}
	})
}
