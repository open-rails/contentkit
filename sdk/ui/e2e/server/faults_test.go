package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/media/token"
)

const faultParent = "01920000-0000-7000-8000-000000000001"
const faultVersion = "01920000-0000-7000-8000-000000000002"

func fixtureFaults() *faults {
	h := &harness{items: newItems(), bucket: "bucket"}
	h.items.put(itemKey("video", faultParent), item{Kind: "video", ID: faultParent, Version: faultVersion})
	return newFaults(h)
}

func TestFaultRulesFollowFlatVersionStorage(t *testing.T) {
	private := "/v1/e2e/video/" + faultVersion + "/private/sha256-" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, path, body string
		matches          bool
	}{
		{"gateway", private, "", true},
		{"bucket multipart", "/bucket/e2e/video/" + faultVersion + "/temp/u-" + faultParent, "", true},
		{"API parent path", "/api/contentkit/v1/media/video/" + faultParent + "/versions/" + faultVersion, "", true},
		{"API parent body", "/api/contentkit/v1/media/upload/presign", `{"ref":{"id":"` + faultParent + `"}}`, true},
		{"other kind", strings.Replace(private, "/video/", "/file/", 1), "", false},
		{"other namespace", strings.Replace(private, "/e2e/", "/other/", 1), "", false},
		{"unknown version", strings.Replace(private, faultVersion, "01920000-0000-7000-8000-000000000003", 1), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixtureFaults()
			if _, err := f.add([]rule{{Item: faultParent, Fault: "drop"}}); err != nil {
				t.Fatal(err)
			}
			hit := f.matchBody(httptest.NewRequest(http.MethodPut, tc.path, nil), tc.body, 1, "drop")
			if (hit != nil) != tc.matches {
				t.Fatalf("matched=%v want=%v", hit != nil, tc.matches)
			}
		})
	}
}

func TestFlatMultipartFaultPreservesSkipAndHitCounts(t *testing.T) {
	f := fixtureFaults()
	if _, err := f.add([]rule{{Item: faultParent, Fault: "drop", Skip: 1, Times: 1}}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPut, "/bucket/e2e/video/"+faultVersion+"/temp/u-"+faultParent+"?partNumber=2", nil)
	if f.match(r, 2, "drop") != nil || f.match(r, 2, "drop") == nil || f.match(r, 2, "drop") != nil {
		t.Fatal("flat version match changed skip/once semantics")
	}
	if got := f.list()[0].Hits; got != 1 {
		t.Fatalf("hits=%d", got)
	}
}

func TestNoCORSFaultMatchesLogicalParentForFlatMedia(t *testing.T) {
	f := fixtureFaults()
	if _, err := f.add([]rule{{Item: faultParent, Fault: "no-cors"}}); err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "https://app.test")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)
	r := httptest.NewRequest(http.MethodGet, "/v1/e2e/video/"+faultVersion+"/private/sha256-"+strings.Repeat("a", 64), nil)
	rec := httptest.NewRecorder()
	f.gatewayProxy(u).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "" || f.list()[0].Hits != 1 {
		t.Fatalf("fault not applied: status=%d headers=%v hits=%d", rec.Code, rec.Header(), f.list()[0].Hits)
	}
}

func TestExpiredFaultPreservesOriginalGrantScope(t *testing.T) {
	f := fixtureFaults()
	ring, err := token.NewRing(token.Key{ID: "test", Secret: []byte(strings.Repeat("s", 32))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.h.ring = ring
	for _, mode := range []string{"folder", "file", "download"} {
		t.Run(mode, func(t *testing.T) {
			key := "e2e/video/" + faultVersion + "/private/sha256-" + strings.Repeat("a", 64) + "-" + faultParent
			scope := "e2e/video/" + faultVersion
			if mode == "file" {
				scope = token.PrivateFileScope(key)
			} else if mode == "download" {
				key = strings.Replace(key, "/private/", "/download/", 1)
				scope = token.DownloadScope(key)
			}
			r := httptest.NewRequest(http.MethodGet, "/v1/"+key, nil)
			q := url.Values{"t": {ring.Sign(scope, time.Now().Add(time.Hour))}}
			r.URL.RawQuery = q.Encode()
			expired := f.expire(r).URL.Query().Get("t")
			if err := ring.VerifyScope(expired, scope, time.Now()); !errors.Is(err, token.ErrExpired) {
				t.Fatalf("fault did not expire token: %v", err)
			}
			if err := ring.VerifyScope(expired, scope, time.Now().Add(-2*time.Hour)); err != nil {
				t.Fatalf("fault changed the original capability scope: %v", err)
			}
		})
	}
}
