package openrails

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConsumerConfigurationAndWire(t *testing.T) {
	var path, token string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		token = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"data":[{"id":"n","merchant_id":"m","customer_id":"c","event_type":"future.new_kind","data":{"amount":"9007199254740993.01"},"created_at":"2026-10-11T00:00:00Z","future":true}]}`)
	}))
	defer server.Close()
	s, err := New(Options{URL: server.URL + "/billing", MerchantID: "m", Consumer: "site-a", Token: func(context.Context) (string, error) { return "app", nil }})
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.pending(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/billing/v1/app/notification-consumers/site-a/events" || token != "Bearer app" || len(events) != 1 || events[0].EventType != "future.new_kind" || !strings.Contains(string(events[0].Data), "9007199254740993.01") {
		t.Fatalf("wire %s %s %+v", path, token, events)
	}
	for _, name := range []string{"", "a/b", "../other", "space here", strings.Repeat("x", 65)} {
		if _, err := New(Options{URL: server.URL, MerchantID: "m", Consumer: name}); err == nil {
			t.Fatalf("accepted consumer %q", name)
		}
	}
}

func TestPendingRejectsInvalidAndCrossMerchantPages(t *testing.T) {
	for name, body := range map[string]string{
		"merchant":       `{"data":[{"id":"n","merchant_id":"other","customer_id":"c","event_type":"payment.failed","data":{},"created_at":"2026-10-11T00:00:00Z"}]}`,
		"customer":       `{"data":[{"id":"n","merchant_id":"m","customer_id":"","event_type":"payment.failed","data":{},"created_at":"2026-10-11T00:00:00Z"}]}`,
		"array payload":  `{"data":[{"id":"n","merchant_id":"m","customer_id":"c","event_type":"payment.failed","data":[],"created_at":"2026-10-11T00:00:00Z"}]}`,
		"wrong envelope": `{"items":[]}`,
		"trailing data":  `{"data":[]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			source, _ := New(Options{URL: server.URL, MerchantID: "m", Consumer: "site"})
			if _, err := source.pending(context.Background(), 100); err == nil {
				t.Fatal("accepted unsafe page")
			}
		})
	}
}

func TestSourceRefusesRedirectsAndSanitizesErrors(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(status)
				fmt.Fprint(w, "secret provider diagnostic")
			}))
			defer server.Close()
			source, _ := New(Options{URL: server.URL, MerchantID: "m", Consumer: "site"})
			err := source.Register(context.Background())
			if err == nil || strings.Contains(err.Error(), "secret") || calls != 1 {
				t.Fatalf("error %v calls %d", err, calls)
			}
		})
	}
}
