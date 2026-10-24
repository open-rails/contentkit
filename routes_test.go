package contentkit

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/contentkit/internal/httpapi"
)

func TestParseRouteEntities(t *testing.T) {
	g, err := ParseRouteGroups([]string{"content", "media"})
	if err != nil || g != ContentRoutes|MediaRoutes {
		t.Fatalf("%v %v", g, err)
	}
	if g, err := ParseRouteGroups([]string{"all"}); err != nil || g != AllRoutes {
		t.Fatalf("%v %v", g, err)
	}
	if _, err := ParseRouteGroups([]string{"nope"}); err == nil {
		t.Fatal("unknown group accepted")
	}
}

// Retired routes are absent from the public contract. Exercise the production
// dispatcher without the contract checker, which correctly rejects requests
// that have no registered route.
func TestRetiredOfferRoutesAreNotRegistered(t *testing.T) {
	modules := make(map[httpapi.Module]http.Handler)
	for _, module := range httpapi.Modules {
		modules[module] = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	}
	h := (&Engine{}).dispatch("/api/contentkit", modules)
	for _, target := range []struct{ method, path string }{
		{http.MethodPut, "/contents/id/offer"},
		{http.MethodDelete, "/contents/id/offer"},
		{http.MethodPut, "/contents/id/versions/version/offer"},
		{http.MethodDelete, "/contents/id/versions/version/offer"},
		{http.MethodPut, "/entities/id/offer"},
		{http.MethodDelete, "/entities/id/offer"},
		{http.MethodGet, "/offers"},
		{http.MethodPost, "/entities/id/join"},
		{http.MethodDelete, "/entities/id/join"},
	} {
		t.Run(target.method+" "+target.path, func(t *testing.T) {
			path := "/v1" + target.path
			if _, ok := httpapi.Match(target.method, path); ok {
				t.Fatal("retired offer route remains registered")
			}
			r := httptest.NewRecorder()
			h.ServeHTTP(r, httptest.NewRequest(target.method, "/api/contentkit"+path, nil))
			if r.Code != http.StatusNotFound {
				t.Fatalf("retired route: got %d, want 404", r.Code)
			}
		})
	}
}
