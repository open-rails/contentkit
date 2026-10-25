package critical_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
)

// This is the approved small HTTP parsing boundary: an actual upstream wire
// supplies gateway/application error metadata. It substitutes no domain state.
func TestGoClientErrorValues(t *testing.T) {
	t.Run("CK-J29-S06_request_identity_retry_delay_and_validation_details", func(t *testing.T) {
		for _, sample := range []struct {
			name, body, requestID string
			delay                 time.Duration
		}{
			{"header_fallback", `{"error":"Slow down","code":"limit_reached","retry_after":2,"details":{"max":4},"blobs":["blob-one"],"field":"files","retryable":true}`, "header-request", 7 * time.Second},
			{"body_identity_and_longer_delay", `{"error":"Slow down","code":"limit_reached","retry_after":11,"request_id":"body-request","details":{"max":4},"blobs":["blob-one"],"field":"files","retryable":true}`, "body-request", 11 * time.Second},
		} {
			t.Run(sample.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.Path != "/v1/contents" {
						t.Error("client sent the wrong public operation")
					}
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Retry-After", "7")
					w.Header().Set("X-Request-ID", "header-request")
					w.WriteHeader(429)
					_, _ = w.Write([]byte(sample.body))
				}))
				defer server.Close()
				_, err := ck.NewClient(server.URL, nil).PostContents(t.Context(), catalog.ContentInput{Kind: "gallery"})
				var failure *ck.Error
				if !errors.As(err, &failure) || failure.Status != 429 || failure.Code != "limit_reached" || failure.Message != "Slow down" || failure.RequestID != sample.requestID || failure.RetryAfter != sample.delay || failure.Field != "files" || !failure.Retryable || string(failure.Details) != `{"max":4}` || len(failure.Blobs) != 1 || failure.Blobs[0] != "blob-one" {
					t.Fatalf("Go transport lost error metadata: %+v", err)
				}
				if !strings.Contains(failure.Error(), "429 limit_reached: Slow down") {
					t.Fatal("formatted Go error lost its useful refusal identity")
				}
			})
		}
	})
}
