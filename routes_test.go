package contentkit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/locationpolicy"
	"github.com/open-rails/contentkit/media"
)

// dispatchEngine serves every module with h. Staff paths never read location
// rules, so no database is reached.
func dispatchEngine(t *testing.T, h http.HandlerFunc, trusted ...netip.Prefix) http.Handler {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	geo, err := locationpolicy.Open("", log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = geo.Close() })
	pool, err := pgxpool.New(t.Context(), "postgres://localhost/unused")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	e := &Engine{cfg: Config{Scope: &Scope{}}, geo: geo, pool: pool, log: log}
	modules := map[httpapi.Module]http.Handler{}
	for _, m := range httpapi.Modules {
		modules[m] = h
	}
	return e.dispatch("", modules, trusted)
}

func errorBody(t *testing.T, w *httptest.ResponseRecorder) media.ErrorReply {
	t.Helper()
	var body media.ErrorReply
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return body
}

func TestDispatchContainsPanics(t *testing.T) {
	h := dispatchEngine(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=60")
		switch r.URL.Query().Get("panic") {
		case "value":
			panic("host port bug")
		case "abort":
			panic(http.ErrAbortHandler)
		case "late":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("partial"))
			panic("after the answer started")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	t.Run("a panic is 500 internal_error", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/manage?panic=value", nil))
		if w.Code != http.StatusInternalServerError || errorBody(t, w).Code != httpapi.CodeInternal {
			t.Fatalf("status %d, body %s", w.Code, w.Body.String())
		}
		if h := w.Header(); h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" || h.Get("Content-Type") != "application/json" {
			t.Fatalf("headers %v", h)
		}
	})
	for name, path := range map[string]string{"an abort passes on": "/v1/manage?panic=abort", "a started answer aborts": "/v1/manage?panic=late"} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if p := recover(); p != http.ErrAbortHandler {
					t.Fatalf("recovered %v, want http.ErrAbortHandler", p)
				}
			}()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
		})
	}
	t.Run("every answer is nosniff", func(t *testing.T) {
		for _, path := range []string{"/v1/manage", "/elsewhere"} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("%s: headers %v", path, w.Header())
			}
		}
	})
}

func TestDispatchResolvesClient(t *testing.T) {
	var seen string
	handler := func(w http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
		w.WriteHeader(http.StatusNoContent)
	}
	request := func(remote string, header http.Header) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/manage", nil)
		r.RemoteAddr, r.Header = remote, header
		return r
	}
	trusted := dispatchEngine(t, handler, netip.MustParsePrefix("10.0.0.0/8"))
	untrusted := dispatchEngine(t, handler)
	for _, c := range []struct {
		name string
		h    http.Handler
		r    *http.Request
		want string
	}{
		{"a trusted proxy names the client", trusted, request("10.1.2.3:4567", http.Header{"X-Forwarded-For": {"198.51.100.1, 203.0.113.7"}}), "203.0.113.7:0"},
		{"an untrusted peer is the client", trusted, request("192.0.2.1:4567", http.Header{"X-Forwarded-For": {"203.0.113.7"}}), "192.0.2.1:0"},
		{"without trusted proxies headers are ignored", untrusted, request("10.1.2.3:4567", http.Header{"X-Forwarded-For": {"203.0.113.7"}}), "10.1.2.3:0"},
		{"a mapped peer is unmapped", untrusted, request("[::ffff:192.0.2.1]:4567", nil), "192.0.2.1:0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c.h.ServeHTTP(w, c.r)
			if w.Code != http.StatusNoContent || seen != c.want {
				t.Fatalf("status %d, RemoteAddr %q, want %q", w.Code, seen, c.want)
			}
		})
	}
	w := httptest.NewRecorder()
	trusted.ServeHTTP(w, request("10.1.2.3:4567", http.Header{"X-Forwarded-For": {"203.0.113.7, garbage"}}))
	if w.Code != http.StatusBadRequest || errorBody(t, w).Code != httpapi.CodeInvalidRequest {
		t.Fatalf("malformed chain: status %d, %s", w.Code, w.Body.String())
	}
}

func TestInProcessPanicIsAnError(t *testing.T) {
	panicky := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("late") != "" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: snapshot\ndata: {}\n\n"))
		}
		panic("host port bug")
	})
	c := &Client{hc: &http.Client{Transport: inprocess{panicky}}, base: inprocessBase}
	if _, err := c.raw(context.Background(), http.MethodGet, "/v1/kinds", nil, nil); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("recorded call: %v", err)
	}
	stream := http.Header{"Accept": {"text/event-stream"}}
	if _, err := c.doHeaders(context.Background(), http.MethodGet, "/v1/media/k/i", nil, nil, stream); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("stream before its answer: %v", err)
	}
	res, err := c.doHeaders(context.Background(), http.MethodGet, "/v1/media/k/i", map[string][]string{"late": {"1"}}, nil, stream)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if _, err := io.ReadAll(res.Body); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("stream after its answer: %v", err)
	}
}

func TestClientTransport(t *testing.T) {
	var sent int
	counting := roundTripper(func(*http.Request) (*http.Response, error) {
		sent++
		return nil, errors.New("sent")
	})
	for _, base := range []string{"http://contentkit.example/api", "ftp://contentkit.example", "/relative"} {
		_, err := NewClient(base, &http.Client{Transport: counting}).GetKinds(context.Background())
		if err == nil || !strings.Contains(err.Error(), "NewClient base") || sent != 0 {
			t.Fatalf("%s: %v, %d requests sent", base, err, sent)
		}
	}
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a redirect carried %q to another origin", r.Header.Get("Authorization"))
	}))
	defer elsewhere.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	bearer := roundTripper(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer secret")
		return http.DefaultTransport.RoundTrip(r)
	})
	for _, base := range []string{origin.URL, strings.Replace(origin.URL, "127.0.0.1", "localhost", 1)} {
		_, err := NewClient(base, &http.Client{Transport: bearer}).GetKinds(context.Background())
		var refused *Error
		if !errors.As(err, &refused) || refused.Status != http.StatusTemporaryRedirect {
			t.Fatalf("%s: %v, want the 307 itself", base, err)
		}
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
