package critical_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
)

func TestShippedBrowserClient(t *testing.T) {
	if _, err := os.Stat(".browser-dist/index.html"); err != nil {
		t.Fatal("build the shipped UI and critical browser harness before this selected test: pnpm --dir ../../sdk/ui build; pnpm --dir ../../sdk/ui exec vite build --config ../../tests/critical/browser/vite.config.mjs")
	}
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a)
	alice, aliceToken := w.person(t, 0)
	bob, bobToken := w.person(t, 0)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(alice.ID), w.writer)
	base, err := url.Parse(a.server.URL)
	must(t, err)
	mux := http.NewServeMux()
	held := &serverHold{test: t.Context()}
	mux.Handle("/content/", held.wrap(httputil.NewSingleHostReverseProxy(base)))
	held.routes(mux)
	mux.HandleFunc("/fixture.png", func(out http.ResponseWriter, _ *http.Request) {
		out.Header().Set("Content-Type", "image/png")
		_, _ = out.Write(imageBytes(t))
	})
	mux.Handle("/", http.FileServer(http.Dir(".browser-dist")))
	server := httptest.NewServer(mux)
	defer server.Close()

	scenarios := []string{"CK-J29-S02", "CK-J29-S03", "CK-J29-S04", "CK-J29-S05", "CK-J29-S06", "CK-J09-S01", "CK-J17-S06"}
	var fixtures []map[string]any
	for _, scenario := range scenarios {
		item := publishedArticle(t, a, "Browser "+scenario)
		_, err := a.client(aliceToken).PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
		must(t, err)
		fixtures = append(fixtures, map[string]any{"scenario": scenario, "fixture": map[string]string{"url": server.URL, "item": item.ID, "alice": alice.ID, "aliceToken": aliceToken, "bob": bob.ID, "bobToken": bobToken}})
	}
	fixture, err := json.Marshal(fixtures)
	must(t, err)
	command := exec.CommandContext(t.Context(), "node", "browser/driver.mjs", "batch")
	command.Env = append(os.Environ(), "CONTENTKIT_BROWSER_FIXTURE="+string(fixture))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("real browser scenarios: %v\n%s", err, output)
	}
	var receipts []struct {
		Scenario string `json:"scenario"`
		Status   string `json:"status"`
	}
	must(t, json.Unmarshal([]byte(strings.TrimSpace(string(output))), &receipts))
	if len(receipts) != len(scenarios) {
		t.Fatalf("browser omitted scenarios: %s", output)
	}
	for i, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			if receipts[i].Scenario != scenario || receipts[i].Status != "passed" {
				t.Fatalf("browser did not execute selected scenario: %+v", receipts[i])
			}
		})
	}
}

// The editor uses the shipped SDK and real handlers, including a stale view
// that learns a vote landed only when its choice edit is refused.
func TestPollChoicesBrowser(t *testing.T) {
	if _, err := os.Stat(".browser-dist/index.html"); err != nil {
		t.Fatal("build the shipped UI and critical browser harness before this selected test")
	}
	w := newWorld(t)
	a := w.apps[0]
	alice, aliceToken := w.person(t, 0)
	bob, bobToken := w.person(t, 0)
	persona, err := w.auth.Persona("app")
	must(t, err)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(alice.ID), persona.OwnerRole())
	poll, err := a.engine.Client().PostPolls(t.Context(), content.PollInput{Question: "Choose a season", Language: "en", Options: []content.PollOptionInput{{Label: "Summer"}, {Label: "Winter", Position: 1}}})
	must(t, err)
	base, err := url.Parse(a.server.URL)
	must(t, err)
	mux := http.NewServeMux()
	mux.Handle("/content/", httputil.NewSingleHostReverseProxy(base))
	mux.Handle("/", http.FileServer(http.Dir(".browser-dist")))
	server := httptest.NewServer(mux)
	defer server.Close()
	fixture, err := json.Marshal(map[string]string{"url": server.URL, "poll": poll.ID, "alice": alice.ID, "aliceToken": aliceToken, "bob": bob.ID, "bobToken": bobToken})
	must(t, err)
	command := exec.CommandContext(t.Context(), "node", "browser/driver.mjs", "CK-J30-S01")
	command.Env = append(os.Environ(), "CONTENTKIT_BROWSER_FIXTURE="+string(fixture))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("poll editor browser: %v\n%s", err, output)
	}
	var receipt struct{ Scenario, Status string }
	must(t, json.Unmarshal([]byte(strings.TrimSpace(string(output))), &receipt))
	if receipt.Scenario != "CK-J30-S01" || receipt.Status != "passed" {
		t.Fatalf("poll editor scenario did not execute: %s", output)
	}
}

// serverHold delays one API response at the test server, so a browser
// scenario can act while a real request is in flight. The request reaches
// the real handler at once; its response waits for the driver's release,
// and a browser that aborts it meanwhile gets nothing, as from any server.
// (Intercepting it in the browser instead left a later request to the same
// URL unanswered.)
type serverHold struct {
	test  context.Context
	mu    sync.Mutex
	armed *heldRequest // the next matching request's
	last  *heldRequest // the latest armed
}

type heldRequest struct {
	method, suffix string
	seen           chan heldResponse
	free, done     chan struct{}
	release        sync.Once
}

type heldResponse struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// take disarms the hold for the request it matches.
func (h *serverHold) take(r *http.Request) *heldRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	held := h.armed
	if held == nil || r.Method != held.method || !strings.HasSuffix(r.URL.Path, held.suffix) {
		return nil
	}
	h.armed = nil
	return held
}

// current is the latest armed hold; it refuses a driver that armed none.
func (h *serverHold) current(out http.ResponseWriter) *heldRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last == nil {
		http.Error(out, "no hold armed", http.StatusConflict)
	}
	return h.last
}

func (h *serverHold) wrap(api http.Handler) http.Handler {
	return http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		held := h.take(r)
		if held == nil {
			api.ServeHTTP(out, r)
			return
		}
		defer close(held.done)
		recorded := httptest.NewRecorder()
		api.ServeHTTP(recorded, r.WithContext(context.WithoutCancel(r.Context())))
		held.seen <- heldResponse{Status: recorded.Code, Body: recorded.Body.Bytes()}
		select {
		case <-held.free:
		case <-h.test.Done():
			return
		}
		if r.Context().Err() != nil {
			return // the browser gave up on it
		}
		for k, v := range recorded.Header() {
			out.Header()[k] = v
		}
		out.WriteHeader(recorded.Code)
		_, _ = out.Write(recorded.Body.Bytes())
	})
}

// routes are the driver's: arm a method and path suffix, wait until the
// request is held (with its real response), release it, wait until done.
func (h *serverHold) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /fixture/hold", func(out http.ResponseWriter, r *http.Request) {
		var arm struct{ Method, Path string }
		if json.NewDecoder(r.Body).Decode(&arm) != nil || arm.Method == "" || arm.Path == "" {
			http.Error(out, "method and path", http.StatusBadRequest)
			return
		}
		held := &heldRequest{method: arm.Method, suffix: arm.Path, seen: make(chan heldResponse, 1), free: make(chan struct{}), done: make(chan struct{})}
		h.mu.Lock()
		h.armed, h.last = held, held
		h.mu.Unlock()
		out.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /fixture/hold/seen", func(out http.ResponseWriter, r *http.Request) {
		held := h.current(out)
		if held == nil {
			return
		}
		select {
		case response := <-held.seen:
			out.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(out).Encode(response)
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("POST /fixture/hold/release", func(out http.ResponseWriter, _ *http.Request) {
		held := h.current(out)
		if held == nil {
			return
		}
		held.release.Do(func() { close(held.free) })
		out.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /fixture/hold/done", func(out http.ResponseWriter, r *http.Request) {
		held := h.current(out)
		if held == nil {
			return
		}
		select {
		case <-held.done:
			out.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	})
}
