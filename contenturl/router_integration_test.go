package contenturl_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/httpapi"
)

func TestRouterCanonicalizes(t *testing.T) {
	ctx, pool, schema, s := setup(t)
	links := put(t, s,
		contenturl.Entry{ContentRef: ref("video", 1), Title: "Night Before the Counteroffensive", Titles: map[string]string{"es": "La noche antes"}},
		contenturl.Entry{ContentRef: ref("gallery", 2), Title: "Hidden Gallery"},
		contenturl.Entry{ContentRef: ref("gallery", 3), Title: "Old Duplicate"},
		contenturl.Entry{ContentRef: ref("comment", 4), Title: "no page"},
		contenturl.Entry{ContentRef: ref("gallery", 5), Title: "Taken Down"},
	)
	video, hidden, dup, removed := links[0].Code, links[1].Code, links[2].Code, links[4].Code
	if err := s.Merge(ctx, ref("gallery", 3), ref("video", 1)); err != nil {
		t.Fatal(err)
	}
	router, err := contenturl.NewRouter(s, contenturl.RouterOptions{
		Routes:    contenturl.Routes{"video": "watch", "gallery": "g"},
		Languages: []string{"en", "es"},
		BaseURL:   "https://hentai0.test/",
		Visibility: func(_ *http.Request, links []contenturl.Link) ([]contenturl.Visibility, error) {
			out := make([]contenturl.Visibility, len(links))
			for i, l := range links {
				switch {
				case l.ContentRef.Equal(ref("gallery", 2)):
					out[i] = contenturl.Hidden
				case l.ContentRef.Equal(ref("gallery", 5)):
					out[i] = contenturl.Gone
				default:
					out[i] = contenturl.Visible
				}
			}
			return out, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var seen contenturl.Decision
	page := router.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = contenturl.FromContext(r.Context())
		w.WriteHeader(http.StatusTeapot) // the host's page or SPA shell
	}))
	canonical := "/en/watch/" + string(video) + "/night-before-the-counteroffensive"
	es := "/es/watch/" + string(video) + "/la-noche-antes"
	lower := strings.ToLower(string(video))
	for _, c := range []struct {
		method, target, accept string
		status                 int
		location               string
	}{
		{"GET", canonical, "", http.StatusTeapot, ""},
		{"HEAD", canonical, "", http.StatusTeapot, ""},
		{"GET", canonical + "?v=K3F9", "", http.StatusTeapot, ""}, // a version's page is its language page
		{"GET", "/en/watch/" + string(video), "", http.StatusMovedPermanently, canonical},
		{"GET", "/en/watch/" + lower + "/stale?t=30&v=K3F9", "", http.StatusMovedPermanently, canonical + "?t=30&v=K3F9"},
		{"GET", "/en/g/" + string(video) + "/night-before-the-counteroffensive/", "", http.StatusMovedPermanently, canonical},
		{"GET", "/es/watch/" + string(video), "", http.StatusMovedPermanently, es},
		{"GET", es, "", http.StatusTeapot, ""},
		// Unprefixed links redirect to the language Accept-Language names, else the default.
		{"GET", "/watch/" + string(video) + "/night-before-the-counteroffensive", "", http.StatusMovedPermanently, canonical},
		{"GET", "/watch/" + lower + "?v=K3F9", "fr-FR, es;q=0.8", http.StatusMovedPermanently, es + "?v=K3F9"},
		{"GET", "/g/" + string(dup) + "/old-duplicate", "", http.StatusMovedPermanently, canonical},
		{"GET", "/en/g/" + string(hidden) + "/hidden-gallery", "", http.StatusTeapot, ""}, // invisible: the host's 404
		{"GET", "/g/" + string(hidden), "", http.StatusTeapot, ""},                        // nor redirected
		{"GET", "/g/" + strings.ToLower(string(removed)), "", http.StatusGone, ""},        // removed: 410, no redirect
		{"GET", "/en/g/" + string(links[3].Code) + "/no-page", "", http.StatusTeapot, ""}, // kind without a route
		{"GET", "/en/watch/00000000A/unknown", "", http.StatusTeapot, ""},                 // unknown code
		{"GET", "/watch/346791971", "", http.StatusTeapot, ""},                            // a legacy numeric id is never a code
		{"GET", "/movie/" + string(video), "", http.StatusTeapot, ""},                     // not a content route
		{"POST", "/watch/" + string(video), "", http.StatusTeapot, ""},                    // writes pass through
		{"GET", canonical + "/comments", "", http.StatusMovedPermanently, canonical},      // extra decorative segments redirect
	} {
		seen = contenturl.Decision{}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(c.method, c.target, nil)
		if c.accept != "" {
			req.Header.Set("Accept-Language", c.accept)
		}
		page.ServeHTTP(rec, req)
		if rec.Code != c.status || rec.Header().Get("Location") != c.location {
			t.Errorf("%s %s = %d %q, want %d %q", c.method, c.target, rec.Code, rec.Header().Get("Location"), c.status, c.location)
		}
		if c.target == canonical && c.method == "GET" {
			if !seen.Matched || seen.Link.Code != video || seen.Path != canonical || !seen.Link.ContentRef.Equal(ref("video", 1)) {
				t.Errorf("decision in context %+v", seen)
			}
			if got := rec.Header().Get("Link"); got != `<https://hentai0.test`+canonical+`>; rel="canonical"` {
				t.Errorf("Link header %q", got)
			}
		}
	}

	api := http.StripPrefix("/api/content-urls", router.Handler(openGuard))
	get := func(target string) (*httptest.ResponseRecorder, contenturl.Resolved) {
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		var body contenturl.Resolved
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec, body
	}
	if rec, body := get("/api/content-urls/" + lower + "?lang=es"); rec.Code != 200 || body.Code != video || body.Path != es ||
		body.ContentKind != "video" || body.ContentID != id(1) || body.Slugs["es"] != "la-noche-antes" {
		t.Fatalf("lookup %d %s", rec.Code, rec.Body.String())
	}
	for _, q := range []string{"?lang=en", "", "?v=K3F9"} {
		if rec, body := get("/api/content-urls/" + string(video) + q); rec.Code != 200 || body.Path != canonical || body.VersionID != "" {
			t.Fatalf("the default language is prefixed (%q): %d %s", q, rec.Code, rec.Body.String())
		}
	}
	if rec, body := get("/api/content-urls/" + string(dup)); rec.Code != 200 || body.Code != video || body.Path != canonical {
		t.Fatalf("merged lookup %d %s", rec.Code, rec.Body.String())
	}
	if rec, body := get("/api/content-urls/" + string(links[3].Code)); rec.Code != 200 || body.Path != "" || body.ContentKind != "comment" {
		t.Fatalf("routeless lookup %d %s", rec.Code, rec.Body.String())
	}
	for target, status := range map[string]int{
		"/api/content-urls/" + string(hidden):             404,
		"/api/content-urls/" + string(removed):            410,
		"/api/content-urls/00000000A":                     404,
		"/api/content-urls/346791971":                     400,
		"/api/content-urls/not-a-code":                    400,
		"/api/content-urls/" + string(video) + "?lang=fr": 400,
	} {
		if rec, _ := get(target); rec.Code != status || !strings.Contains(rec.Body.String(), `"code":`) {
			t.Errorf("GET %s = %d %s, want %d", target, rec.Code, rec.Body.String(), status)
		}
	}

	if err := putAliases(ctx, pool, schema,
		alias("hentai0-legacy", "video", "346791971", "", ref("video", 1)),
		alias("doujins-legacy", "object-token", "ab12cd34", "12", ref("video", 1)),
		alias("hentai0-legacy", "video", "5", "", ref("gallery", 5)),
		alias("hentai0-legacy", "video", "2", "", ref("gallery", 2)),
	); err != nil {
		t.Fatal(err)
	}
	key := func(source, kind, k string) contenturl.AliasKey {
		return contenturl.AliasKey{Source: source, Kind: kind, Key: k}
	}
	lookup := contenturl.AliasLookup{Aliases: []contenturl.AliasKey{key("hentai0-legacy", "video", "346791971"), key("doujins-legacy", "object-token", "ab12cd34"),
		key("hentai0-legacy", "video", "5"), key("hentai0-legacy", "video", "2"), key("hentai0-legacy", "video", "404")}}
	post := func(target string, body any) (*httptest.ResponseRecorder, contenturl.AliasResults) {
		b, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, httptest.NewRequest("POST", target, bytes.NewReader(b)))
		var out contenturl.AliasResults
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out
	}
	rec, got := post("/api/content-urls/aliases?lang=es", lookup)
	if rec.Code != 200 || len(got.Aliases) != 5 {
		t.Fatalf("aliases %d %s", rec.Code, rec.Body)
	}
	esPath := es
	for i, want := range []struct{ status, path, locator string }{
		{contenturl.AliasFound, esPath, ""}, {contenturl.AliasFound, esPath, "12"}, {contenturl.AliasGone, "", ""}, {contenturl.AliasNotFound, "", ""}, {contenturl.AliasNotFound, "", ""},
	} {
		a := got.Aliases[i]
		path := ""
		if a.Resolved != nil {
			path = a.Resolved.Path
		}
		if a.AliasKey != lookup.Aliases[i] || a.Status != want.status || path != want.path || a.Locator != want.locator ||
			(a.Status == contenturl.AliasFound) != (a.Resolved != nil && a.Resolved.Code == video) {
			t.Errorf("alias %d = %+v, want %+v", i, a, want)
		}
	}
	tooMany := contenturl.AliasLookup{Aliases: make([]contenturl.AliasKey, contenturl.MaxAliases+1)}
	for i := range tooMany.Aliases {
		tooMany.Aliases[i] = key("hentai0-legacy", "video", fmt.Sprint(i))
	}
	for target, body := range map[string]any{
		"/api/content-urls/aliases?lang=fr": lookup,
		"/api/content-urls/aliases":         contenturl.AliasLookup{Aliases: []contenturl.AliasKey{key("Hentai0", "video", "1")}},
		"/api/content-urls/aliases?x=1":     map[string]any{"aliases": []any{}, "lang": "es"},
		"/api/content-urls/aliases?n=101":   tooMany,
	} {
		if rec, _ := post(target, body); rec.Code != 400 || !strings.Contains(rec.Body.String(), `"code":"invalid_request"`) {
			t.Errorf("POST %s = %d %s, want 400", target, rec.Code, rec.Body)
		}
	}
	if rec, got := post("/api/content-urls/aliases", contenturl.AliasLookup{}); rec.Code != 200 || got.Aliases == nil || len(got.Aliases) != 0 {
		t.Errorf("empty lookup %d %s", rec.Code, rec.Body)
	}
	if path, ok := router.Path(contenturl.Link{ContentRef: contentref.New(tenant, "gallery", id(9)), Code: "0000000AB", Slug: "x"}, ""); !ok || path != "/en/g/0000000AB/x" {
		t.Fatalf("Path %q %v", path, ok)
	}
}

// openGuard admits every request as it is; the tests decide visibility.
func openGuard(_ httpapi.Spec, next http.Handler) http.Handler { return next }

// A Preview verdict resolves for its viewer only: never cached or indexed.
func TestRouterPreviewIsPrivate(t *testing.T) {
	_, _, _, s := setup(t)
	links := put(t, s, contenturl.Entry{ContentRef: ref("gallery", 1), Title: "Draft"})
	router, err := contenturl.NewRouter(s, contenturl.RouterOptions{Routes: contenturl.Routes{"gallery": "g"}, Languages: []string{"en"},
		Visibility: func(r *http.Request, links []contenturl.Link) ([]contenturl.Visibility, error) {
			v := contenturl.Hidden
			if r.Header.Get("X-Editor") != "" {
				v = contenturl.Preview
			}
			return []contenturl.Visibility{v}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	canonical := "/en/g/" + string(links[0].Code) + "/draft"
	var seen contenturl.Decision
	page := router.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = contenturl.FromContext(r.Context())
		w.WriteHeader(http.StatusTeapot)
	}))
	api := router.Handler(openGuard)
	for _, editor := range []bool{false, true} {
		req := httptest.NewRequest("GET", canonical, nil)
		lookup := httptest.NewRequest("GET", "/"+string(links[0].Code), nil)
		if editor {
			req.Header.Set("X-Editor", "1")
			lookup.Header.Set("X-Editor", "1")
		}
		seen = contenturl.Decision{}
		rec, found := httptest.NewRecorder(), httptest.NewRecorder()
		page.ServeHTTP(rec, req)
		api.ServeHTTP(found, lookup)
		private := rec.Header().Get("Cache-Control") == "private, no-store" && rec.Header().Get("X-Robots-Tag") == "noindex" &&
			found.Header().Get("Cache-Control") == "private, no-store" && found.Header().Get("X-Robots-Tag") == "noindex"
		if seen.Matched != editor || seen.Private != editor || private != editor || (found.Code == http.StatusOK) != editor {
			t.Errorf("editor=%v: decision %+v, page headers %v, lookup %d %v", editor, seen, rec.Header(), found.Code, found.Header())
		}
	}
}

func TestNewRouterValidates(t *testing.T) {
	_, _, _, s := setup(t)
	for _, opts := range []contenturl.RouterOptions{
		{},
		{Routes: contenturl.Routes{"video": "watch"}},
		{Routes: contenturl.Routes{"video": "watch"}, Languages: []string{"watch"}},
		{Routes: contenturl.Routes{"video": "watch"}, Languages: []string{"en"}, BaseURL: "hentai0.test"},
	} {
		if _, err := contenturl.NewRouter(s, opts); !errors.Is(err, contenturl.ErrInvalid) {
			t.Errorf("NewRouter(%+v) = %v", opts, err)
		}
	}
	if _, err := contenturl.NewRouter(nil, contenturl.RouterOptions{Routes: contenturl.Routes{"video": "watch"}}); !errors.Is(err, contenturl.ErrInvalid) {
		t.Errorf("nil store: %v", err)
	}
}
