package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEventLogConfigurationAndWire(t *testing.T) {
	var path, token, after string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, token, after = r.URL.Path, r.Header.Get("Authorization"), r.URL.Query().Get("after")
		e := event("n", "c")
		e.EventType = "future.new_kind"
		e.Data = json.RawMessage(`{"amount":"9007199254740993.01","customer_id":"c","account_id":"c","account_issuer":"","downgrade_applied":"future-kind-value"}`)
		_ = json.NewEncoder(w).Encode(EventPage{Events: []Event{e}, NextCursor: "next", RetentionSeconds: 90 * 86400})
	}))
	defer server.Close()
	s, err := New(Options{URL: server.URL + "/billing", MerchantID: "m", Token: func(context.Context) (string, error) { return "app", nil }})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.read(t.Context(), "cursor+with/slash", 100)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/billing/v1/app/events" || token != "Bearer app" || after != "cursor+with/slash" || len(page.Events) != 1 || !strings.Contains(string(page.Events[0].Data), "9007199254740993.01") {
		t.Fatalf("wire %s %s %s %+v", path, token, after, page)
	}
	for _, url := range []string{"", "ftp://example.test", "https://user:secret@example.test", "https://example.test?x=y", "https://example.test/#fragment"} {
		if _, err := New(Options{URL: url, MerchantID: "m"}); err == nil {
			t.Fatalf("accepted %q", url)
		}
	}
}

func TestReadRejectsInvalidAndCrossMerchantPages(t *testing.T) {
	good := EventPage{Events: []Event{event("n", "c")}, NextCursor: "next", RetentionSeconds: 90 * 86400}
	tests := map[string]func(*EventPage){
		"merchant":          func(p *EventPage) { p.Events[0].MerchantID = "other" },
		"customer":          func(p *EventPage) { p.Events[0].Subject.ID = "other" },
		"array payload":     func(p *EventPage) { p.Events[0].Data = json.RawMessage("[]") },
		"unknown schema":    func(p *EventPage) { p.Events[0].Version = 2 },
		"missing cursor":    func(p *EventPage) { p.NextCursor = "" },
		"missing retention": func(p *EventPage) { p.RetentionSeconds = 0 },
		"missing events":    func(p *EventPage) { p.Events = nil },
		"malformed renewal": func(p *EventPage) {
			p.Events[0].EventType = "premium_renewed"
			p.Events[0].Data = json.RawMessage(`{"customer_id":"c","account_id":"c","account_issuer":"","downgrade_applied":"invalid"}`)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			page := good
			page.Events = append([]Event(nil), good.Events...)
			mutate(&page)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(page) }))
			defer server.Close()
			source, _ := New(Options{URL: server.URL, MerchantID: "m"})
			if _, err := source.read(t.Context(), "", 100); err == nil {
				t.Fatal("accepted unsafe page")
			}
		})
	}
	for _, body := range []string{`{"items":[]}`, `{"events":[],"next_cursor":"n","retention_seconds":1} {}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		source, _ := New(Options{URL: server.URL, MerchantID: "m"})
		if _, err := source.read(t.Context(), "", 100); err == nil {
			t.Fatal("accepted bad envelope")
		}
		server.Close()
	}
}

func TestSourceRefusesRedirectsAndSanitizesErrors(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusForbidden, http.StatusGone} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(status)
				fmt.Fprint(w, "secret provider diagnostic")
			}))
			defer server.Close()
			source, _ := New(Options{URL: server.URL, MerchantID: "m"})
			_, err := source.read(t.Context(), "", 100)
			if err == nil || strings.Contains(err.Error(), "secret") || calls != 1 {
				t.Fatalf("error %v calls %d", err, calls)
			}
			if status == 410 && !errors.Is(err, ErrCursorExpired) {
				t.Fatalf("missing expiry code: %v", err)
			}
		})
	}
}
