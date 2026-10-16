package contentkit_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ck "github.com/open-rails/contentkit"
)

func TestClientPreservesActionableErrorFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.Header().Set("X-Request-ID", "request-one")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"rate_limited","error":"try later","retry_after":3,"field":"archive.size","details":{"max_bytes":100},"blobs":["expired"]}`))
	}))
	defer server.Close()
	_, err := ck.NewClient(server.URL, nil).GetKinds(context.Background())
	var api *ck.Error
	if !errors.As(err, &api) || api.Status != http.StatusTooManyRequests || api.Code != "rate_limited" || api.RetryAfter != 7*time.Second ||
		api.RequestID != "request-one" || api.Field != "archive.size" || len(api.Blobs) != 1 || api.Blobs[0] != "expired" || !strings.Contains(string(api.Details), "max_bytes") {
		t.Fatalf("error lost fields: %#v, %v", api, err)
	}
}

func TestClientLabelsNonJSONGatewayFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>proxy unavailable</html>"))
	}))
	defer server.Close()
	_, err := ck.NewClient(server.URL, nil).GetKinds(context.Background())
	var api *ck.Error
	if !errors.As(err, &api) || api.Message != "Bad Gateway" || api.Status != http.StatusBadGateway {
		t.Fatalf("gateway error=%#v, %v", api, err)
	}
}
