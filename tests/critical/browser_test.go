package critical_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
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
	mux.Handle("/content/", httputil.NewSingleHostReverseProxy(base))
	mux.HandleFunc("/fixture.png", func(out http.ResponseWriter, _ *http.Request) {
		out.Header().Set("Content-Type", "image/png")
		_, _ = out.Write(imageBytes(t))
	})
	mux.Handle("/", http.FileServer(http.Dir(".browser-dist")))
	server := httptest.NewServer(mux)
	defer server.Close()

	scenarios := []string{"CK-J29-S02", "CK-J29-S03", "CK-J29-S04", "CK-J29-S05", "CK-J29-S06", "CK-J09-S01"}
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
