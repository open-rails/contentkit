package openrails

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchScopeAndForwardCompatibleData(t *testing.T) {
	var gotPath, gotAuth, gotCursor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCursor = r.URL.Query().Get("cursor")
		fmt.Fprint(w, `{"data":[{"id":"n1","merchant_id":"m1","customer_id":"c1","event_type":"future.new_kind","data":{"amount":"9007199254740993.01","extra":{"new":true}},"created_at":"2026-10-11T15:00:00Z","seen":true,"future":"ignored"}],"next_cursor":"next"}`)
	}))
	defer srv.Close()
	source, err := New(Options{URL: srv.URL + "/billing", MerchantID: "m1", Token: func(context.Context) (string, error) { return "app-token", nil }})
	if err != nil {
		t.Fatal(err)
	}
	page, err := source.Fetch(context.Background(), "c1", "start")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/billing/v1/app/customers/c1/notifications" || gotAuth != "Bearer app-token" || gotCursor != "start" {
		t.Fatalf("request %q %q %q", gotPath, gotAuth, gotCursor)
	}
	if len(page.Data) != 1 || page.Data[0].EventType != "future.new_kind" || !strings.Contains(string(page.Data[0].Data), "9007199254740993.01") || page.NextCursor == nil || *page.NextCursor != "next" {
		t.Fatalf("page = %+v", page)
	}
}

func TestFetchRejectsCrossScopeAndMalformedPages(t *testing.T) {
	for name, body := range map[string]string{
		"merchant":       `{"data":[{"id":"n","merchant_id":"other","customer_id":"c","event_type":"payment.failed","data":{},"created_at":"2026-10-11T00:00:00Z"}]}`,
		"customer":       `{"data":[{"id":"n","merchant_id":"m","customer_id":"other","event_type":"payment.failed","data":{},"created_at":"2026-10-11T00:00:00Z"}]}`,
		"array payload":  `{"data":[{"id":"n","merchant_id":"m","customer_id":"c","event_type":"payment.failed","data":[],"created_at":"2026-10-11T00:00:00Z"}]}`,
		"wrong envelope": `{"items":[]}`,
		"cursor loop":    `{"data":[],"next_cursor":"start"}`,
		"trailing data":  `{"data":[]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer srv.Close()
			source, _ := New(Options{URL: srv.URL, MerchantID: "m"})
			if _, err := source.Fetch(context.Background(), "c", "start"); err == nil {
				t.Fatal("accepted unsafe source page")
			}
		})
	}
}

func TestFetchDoesNotFollowRedirectOrExposeSourceBody(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(status)
				fmt.Fprint(w, "secret provider diagnostic")
			}))
			defer srv.Close()
			source, _ := New(Options{URL: srv.URL, MerchantID: "m"})
			_, err := source.Fetch(context.Background(), "c", "")
			if err == nil || strings.Contains(err.Error(), "secret") || calls != 1 {
				t.Fatalf("error %v, calls %d", err, calls)
			}
		})
	}
}
