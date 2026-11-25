package contentkit

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/locationpolicy"
)

func TestLocationRequestReaderBoundary(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	geo, err := locationpolicy.Open("", log)
	if err != nil {
		t.Fatal(err)
	}
	defer geo.Close()
	pool, err := pgxpool.New(t.Context(), "postgres://localhost/unused")
	if err != nil {
		t.Fatal(err)
	}
	// A closed pool makes an attempted policy read fail without a database.
	pool.Close()
	e := &Engine{cfg: Config{Scope: &Scope{}}, geo: geo, pool: pool, log: log}
	modules := map[httpapi.Module]http.Handler{}
	for _, m := range httpapi.Modules {
		modules[m] = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	}
	h := e.dispatch("", modules, nil)
	for _, path := range []string{
		"/v1/manage", "/v1/manage/article", "/v1/manage/article/count",
		"/v1/versions?ids=version", "/v1/events", "/v1/comments/admin",
		"/v1/moderation/held", "/v1/moderation/location-rules",
		"/v1/manage/search-suggestions", "/v1/manage/entities", "/v1/manage/channels",
	} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusNoContent {
				t.Fatalf("staff route read location policy: status %d, %s", w.Code, w.Body.String())
			}
		})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/contents/item", nil))
	if w.Code != http.StatusInternalServerError || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("reader refusal: status %d, headers %v", w.Code, w.Header())
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != httpapi.CodeInternal || body.Error != httpapi.Message(httpapi.CodeInternal) {
		t.Fatalf("reader refusal body: %+v", body)
	}
}
