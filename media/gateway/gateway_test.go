package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
)

const (
	testHost   = "media.test"
	testBucket = "bucket"
	testETag   = `"blob-etag"`
)

var testScope = layout.Scope{
	TenantID: "01900000-0000-7000-8000-000000000001",
	AppID:    "01900000-0000-7000-8000-000000000002",
}

// fakeBucket answers the gateway's S3 reads: no erasure fence, one object that
// honors Range and If-None-Match.
func fakeBucket(t *testing.T) *httptest.Server {
	t.Helper()
	body := strings.Repeat("x", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/public/"+layout.FenceName) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", testETag)
		w.Header().Set("Content-Type", "image/webp")
		switch {
		case r.Header.Get("If-None-Match") == testETag:
			w.WriteHeader(http.StatusNotModified)
		case r.Header.Get("Range") != "":
			w.Header().Set("Content-Range", "bytes 0-9/64")
			w.WriteHeader(http.StatusPartialContent)
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte(body[:10]))
			}
		default:
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte(body))
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newGateway(t *testing.T) (*Handler, token.Ring, string) {
	t.Helper()
	ring, err := token.NewRing(token.Key{ID: "k1", Secret: []byte(strings.Repeat("s", 32))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{
		Endpoint:        fakeBucket(t).URL,
		AccessKeyID:     "id",
		SecretAccessKey: "secret",
		Ring:            ring,
		Routes:          []Route{{Host: testHost, Scope: testScope, Bucket: testBucket}},
	})
	if err != nil {
		t.Fatal(err)
	}
	key := testScope.Prefix("gallery", "01900000-0000-7000-8000-000000000003", "") +
		"private/" + layout.BlobName(make([]byte, 32), "01900000-0000-7000-8000-000000000004")
	return h, ring, key
}

func get(h http.Handler, method, key string, query url.Values, hdr map[string]string, cookie string) *httptest.ResponseRecorder {
	target := "http://" + testHost + layout.URLPrefix + key
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	r := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: token.CookieName, Value: cookie})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// maxAge parses "private, max-age=N, immutable" and fails on anything else.
func maxAge(t *testing.T, cacheControl string) int {
	t.Helper()
	parts := strings.Split(cacheControl, ", ")
	if len(parts) != 3 || parts[0] != "private" || parts[2] != "immutable" || !strings.HasPrefix(parts[1], "max-age=") {
		t.Fatalf("Cache-Control = %q, want private, max-age=N, immutable", cacheControl)
	}
	n, err := strconv.Atoi(strings.TrimPrefix(parts[1], "max-age="))
	if err != nil {
		t.Fatalf("Cache-Control = %q: %v", cacheControl, err)
	}
	return n
}

// A cached private response must not outlive the token that authorized it,
// otherwise expiry, key removal and the deletion fence never run for a browser
// that already holds the bytes.
func TestPrivateCacheLifetimeFollowsTokenExpiry(t *testing.T) {
	h, ring, key := newGateway(t)
	const ttl = 10 * time.Minute
	tok := ring.Sign(token.PrivateFileScope(key), time.Now().Add(ttl).Truncate(time.Second))

	for _, mode := range []string{"url", "cookie"} {
		t.Run(mode, func(t *testing.T) {
			query, cookie := url.Values{}, ""
			if mode == "url" {
				query.Set("t", tok)
			} else {
				cookie = tok
			}
			for _, c := range []struct {
				name, method string
				hdr          map[string]string
				status       int
			}{
				{"200", http.MethodGet, nil, http.StatusOK},
				{"206", http.MethodGet, map[string]string{"Range": "bytes=0-9"}, http.StatusPartialContent},
				{"304", http.MethodGet, map[string]string{"If-None-Match": testETag}, http.StatusNotModified},
				{"HEAD", http.MethodHead, nil, http.StatusOK},
			} {
				w := get(h, c.method, key, query, c.hdr, cookie)
				if w.Code != c.status {
					t.Fatalf("%s: status = %d, want %d", c.name, w.Code, c.status)
				}
				if n := maxAge(t, w.Header().Get("Cache-Control")); n < int(ttl/time.Second)-5 || n > int(ttl/time.Second) {
					t.Fatalf("%s: max-age = %d, want about %d (the token's remaining life)", c.name, n, int(ttl/time.Second))
				}
			}
		})
	}
}

func TestPrivateCacheLifetimeShrinksWithRemainingTokenLife(t *testing.T) {
	h, ring, key := newGateway(t)
	tok := ring.Sign(token.PrivateFileScope(key), time.Now().Add(3*time.Second).Truncate(time.Second))
	w := get(h, http.MethodGet, key, url.Values{"t": {tok}}, nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if n := maxAge(t, w.Header().Get("Cache-Control")); n > 3 {
		t.Fatalf("max-age = %d for a token with at most 3s left", n)
	}
}

func TestExpiredTokenIsStillAnUncacheable404(t *testing.T) {
	h, ring, key := newGateway(t)
	tok := ring.Sign(token.PrivateFileScope(key), time.Now().Add(-time.Minute))
	w := get(h, http.MethodGet, key, url.Values{"t": {tok}}, nil, "")
	if w.Code != http.StatusNotFound || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expired token: %d, Cache-Control %q", w.Code, w.Header().Get("Cache-Control"))
	}
}

func TestWholeItemTokenBoundsCacheToo(t *testing.T) {
	h, ring, key := newGateway(t)
	k, _ := layout.Parse(key)
	tok := ring.Sign(strings.TrimSuffix(k.Folder(), "/"), time.Now().Add(time.Hour).Truncate(time.Second))
	w := get(h, http.MethodGet, key, nil, nil, tok)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if n := maxAge(t, w.Header().Get("Cache-Control")); n < 3590 || n > 3600 {
		t.Fatalf("max-age = %d, want about 3600", n)
	}
}
