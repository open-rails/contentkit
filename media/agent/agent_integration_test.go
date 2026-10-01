package agent_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/agent"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
)

var (
	k0 = token.Key{ID: "k0", Secret: bytes.Repeat([]byte{7}, 32)}
	k1 = token.Key{ID: "k1", Secret: bytes.Repeat([]byte{1}, 32)}
	k2 = token.Key{ID: "k2", Secret: bytes.Repeat([]byte{2}, 32)}
)

const (
	host   = "media.test"
	origin = "https://doujins.test"
	bodyA  = "0123456789abcdefghij"
)

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256-" + hex.EncodeToString(sum[:])
}

func ring(t *testing.T, cur token.Key, prev *token.Key) token.Ring {
	t.Helper()
	r, err := token.NewRing(cur, prev)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type fixture struct {
	env *s3test.Env
	ns  string
}

// seed writes one gallery namespace: item 456 with a cover and two blobs,
// item 789 without a cover, and the kind's default cover.
func seed(t *testing.T) *fixture {
	t.Helper()
	env := s3test.Open(t)
	f := &fixture{env: env, ns: env.Tenant}
	for key, body := range map[string]string{
		"gallery/456/public/cover-460.webp":      "cover",
		"gallery/_default/public/cover-460.webp": "default cover",
		"gallery/456/private/" + sha("a"):        bodyA,
		"gallery/456/private/" + sha("b"):        "bee",
		"gallery/789/private/" + sha("c"):        "other",
		"gallery/456/manifest.json":              "{}",
		"gallery/456/temp/" + sha("t"):           "temp",
	} {
		if _, err := env.Store.Put(context.Background(), f.ns+"/"+key, strings.NewReader(body), int64(len(body)),
			media.PutOptions{ContentType: "image/webp"}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// path is the URL path of "{kind}/{id}/{area}/{name}" in the fixture's namespace.
func (f *fixture) path(rest string) string { return "/v1/" + f.ns + "/" + rest }

// key is the object key of "{kind}/{id}/{area}/{name}".
func (f *fixture) key(rest string) string { return f.ns + "/" + rest }

func (f *fixture) config(t *testing.T) agent.Config {
	return agent.Config{
		Endpoint: f.env.Config.Endpoint, Bucket: f.env.Config.Bucket, Region: f.env.Config.Region,
		AccessKeyID: f.env.Config.AccessKeyID, SecretAccessKey: f.env.Config.SecretAccessKey,
		Ring:     ring(t, k2, &k1),
		Hosts:    map[string][]string{host: {"accounts", f.ns}, "other.test": {"accounts"}},
		Origins:  []string{origin},
		Defaults: []layout.Default{{Namespace: f.ns, Kind: "gallery", Names: []string{"cover-{w}.webp"}}},
	}
}

func serve(t *testing.T, cfg agent.Config) *httptest.Server {
	t.Helper()
	h, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

type result struct {
	status int
	header http.Header
	body   string
}

// do sends method path to srv with Host media.test unless hdr names another.
func do(t *testing.T, srv *httptest.Server, method, path string, hdr map[string]string) result {
	t.Helper()
	u, err := url.Parse(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	req := &http.Request{Method: method, URL: u, Header: http.Header{}, Host: host}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{resp.StatusCode, resp.Header, string(b)}
}

func withQuery(path string, kv ...string) string {
	q := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	return path + "?" + q.Encode()
}

func cookie(tok string) map[string]string {
	return map[string]string{"Cookie": token.CookieName + "=" + tok}
}

func expect(t *testing.T, r result, status int, body string) {
	t.Helper()
	if r.status != status || r.body != body {
		t.Fatalf("got %d %q; want %d %q", r.status, r.body, status, body)
	}
}

// same fails unless got and want match in status, body and every header but Date.
func same(t *testing.T, got, want result) {
	t.Helper()
	g, w := got.header.Clone(), want.header.Clone()
	g.Del("Date")
	w.Del("Date")
	if got.status != want.status || got.body != want.body || !reflect.DeepEqual(g, w) {
		t.Fatalf("response differs from not-found:\n got %d %q %v\nwant %d %q %v", got.status, got.body, g, want.status, want.body, w)
	}
}

func TestAgent(t *testing.T) {
	f := seed(t)
	srv := serve(t, f.config(t))
	cur := ring(t, k2, nil)
	exp := token.Expiry(time.Now(), time.Hour, 0)
	item := cur.Sign(token.ItemScope(f.ns, "gallery", "456"), exp)
	blobA, blobB := f.path("gallery/456/private/"+sha("a")), f.path("gallery/456/private/"+sha("b"))
	fileA := cur.Sign(token.FileScope(f.key("gallery/456/private/"+sha("a"))), exp)
	cover := f.path("gallery/456/public/cover-460.webp")

	// Every refusal is byte-identical to an authorized request for a missing blob.
	notFound := func(method string) result {
		return do(t, srv, method, f.path("gallery/456/private/"+sha("missing")), cookie(item))
	}
	nf := notFound("GET")
	if nf.status != 404 || nf.header.Get("Cache-Control") != "no-store" || nf.header.Get("Cross-Origin-Resource-Policy") != "same-site" ||
		nf.header.Get("X-Content-Type-Options") != "nosniff" || nf.header.Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
		t.Fatalf("not-found: %d %v", nf.status, nf.header)
	}
	denied := func(t *testing.T, r result) { t.Helper(); same(t, r, nf) }

	t.Run("public object", func(t *testing.T) {
		r := do(t, srv, "GET", cover, nil)
		expect(t, r, 200, "cover")
		for name, want := range map[string]string{
			"Cache-Control": "public, max-age=300, stale-while-revalidate=86400", "Content-Type": "image/webp", "Content-Length": "5",
			"Cross-Origin-Resource-Policy": "same-site", "X-Content-Type-Options": "nosniff",
			"Content-Security-Policy": "default-src 'none'; sandbox", "Vary": "Origin",
		} {
			if got := r.header.Get(name); got != want {
				t.Errorf("%s: %q, want %q", name, got, want)
			}
		}
		if r.header.Get("ETag") == "" || r.header.Get("Last-Modified") == "" || r.header.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("public headers: %v", r.header)
		}
		expect(t, do(t, srv, "GET", cover, map[string]string{"If-None-Match": r.header.Get("ETag")}), 304, "")
	})

	t.Run("default fallback", func(t *testing.T) {
		r := do(t, srv, "GET", f.path("gallery/789/public/cover-460.webp"), nil)
		expect(t, r, 200, "default cover")
		if r.header.Get("Cache-Control") != "public, max-age=300, stale-while-revalidate=86400" {
			t.Fatalf("default headers: %v", r.header)
		}
		denied(t, do(t, srv, "GET", f.path("gallery/789/public/banner.webp"), nil))         // no template
		denied(t, do(t, srv, "GET", f.path("gallery/789/public/cover-920.webp"), nil))      // no default object
		denied(t, do(t, srv, "GET", f.path("video/789/public/cover-460.webp"), nil))        // another kind
		denied(t, do(t, srv, "GET", f.path("gallery/_default/public/cover-460.webp"), nil)) // only through the fallback
	})

	t.Run("hosts", func(t *testing.T) {
		expect(t, do(t, srv, "GET", cover, map[string]string{"Host": "MEDIA.test:443"}), 200, "cover")
		denied(t, do(t, srv, "GET", cover, map[string]string{"Host": "other.test"})) // the namespace is not served there
		denied(t, do(t, srv, "GET", cover, map[string]string{"Host": "unknown.test"}))
		denied(t, do(t, srv, "GET", blobA, map[string]string{"Host": "other.test", "Cookie": "mt=" + item}))
	})

	t.Run("private", func(t *testing.T) {
		r := do(t, srv, "GET", blobA, cookie(item))
		expect(t, r, 200, bodyA)
		if r.header.Get("Cache-Control") != "private, max-age=31536000, immutable" || r.header.Get("Content-Disposition") != "" ||
			r.header.Get("Cross-Origin-Resource-Policy") != "same-site" {
			t.Fatalf("private headers: %v", r.header)
		}
		expect(t, do(t, srv, "GET", blobB, cookie(item)), 200, "bee")
		expect(t, do(t, srv, "GET", withQuery(blobB, "t", item), nil), 200, "bee")
		expect(t, do(t, srv, "GET", withQuery(blobA, "t", fileA), nil), 200, bodyA)
		other := cur.Sign(token.ItemScope(f.ns, "gallery", "789"), exp)
		expect(t, do(t, srv, "GET", blobA, map[string]string{"Cookie": "mt=" + other + "; mt=" + item}), 200, bodyA)
		expect(t, do(t, srv, "GET", withQuery(blobA, "t", ring(t, k1, nil).Sign(token.ItemScope(f.ns, "gallery", "456"), exp)), nil), 200, bodyA)
		for name, r := range map[string]result{
			"no token":       do(t, srv, "GET", blobA, nil),
			"garbage":        do(t, srv, "GET", withQuery(blobA, "t", "garbage"), nil),
			"other item":     do(t, srv, "GET", withQuery(blobA, "t", other), cookie(other)),
			"other file":     do(t, srv, "GET", withQuery(blobB, "t", fileA), cookie(fileA)),
			"folder scope":   do(t, srv, "GET", blobA, cookie(cur.Sign(f.key("gallery/456/private/"), exp))),
			"expired":        do(t, srv, "GET", blobA, cookie(cur.Sign(token.ItemScope(f.ns, "gallery", "456"), time.Now().Add(-time.Second)))),
			"unknown key":    do(t, srv, "GET", withQuery(blobA, "t", ring(t, k0, nil).Sign(token.ItemScope(f.ns, "gallery", "456"), exp)), nil),
			"other cookie":   do(t, srv, "GET", blobA, map[string]string{"Cookie": "other=" + item}),
			"other blob 789": do(t, srv, "GET", f.path("gallery/789/private/"+sha("c")), cookie(item)),
		} {
			t.Run(name, func(t *testing.T) { denied(t, r) })
		}
	})

	t.Run("download name", func(t *testing.T) {
		name := `Title "ep" (1080p) ✓.mp4`
		dl := cur.Sign(token.DownloadScope(f.key("gallery/456/private/"+sha("a")), name), exp)
		r := do(t, srv, "GET", withQuery(blobA, "t", dl, "dl", name), nil)
		expect(t, r, 200, bodyA)
		if got := r.header.Get("Content-Disposition"); got != token.Attachment(name) {
			t.Fatalf("Content-Disposition %q", got)
		}
		denied(t, do(t, srv, "GET", withQuery(blobA, "t", item, "dl", name), nil))
		denied(t, do(t, srv, "GET", withQuery(blobA, "t", fileA, "dl", name), nil))
		denied(t, do(t, srv, "GET", withQuery(blobA, "t", dl, "dl", "Other.mp4"), nil))
		denied(t, do(t, srv, "GET", withQuery(blobA, "t", dl, "dl", ""), nil))
		denied(t, do(t, srv, "GET", withQuery(blobA, "t", dl), nil))
		denied(t, do(t, srv, "GET", withQuery(blobA, "dl", name), cookie(dl)))
		denied(t, do(t, srv, "GET", withQuery(blobA, "dl", name), cookie(item)))
	})

	t.Run("refused paths", func(t *testing.T) {
		for _, p := range []string{
			f.path("gallery/456/temp/" + sha("t")),
			f.path("gallery/456/manifest.json"),
			f.path("gallery/456/"),
			"/v1/" + f.ns + "/gallery/",
			f.path("gallery/_default/public/cover-460.webp"),
			f.path("gallery/456/public/cover-460.webp/x"),
			f.path("gallery/456/cover-460.webp"),
			f.path("gallery/456/other/" + sha("a")),
			f.path("gallery/456/private/cover-460.webp"),
			f.path("gallery/456/private/" + strings.ToUpper(sha("a"))),
			f.path("gallery/456/private/../public/cover-460.webp"),
			f.path("gallery/456/public/%63over-460.webp"),
			f.path("gallery/456/private%2F" + sha("a")),
			f.path("gallery/456/public/.cover-460.webp"),
			f.path("gallery//public/cover-460.webp"),
			"/v1//" + f.ns + "/gallery/456/public/cover-460.webp",
			"/v2/" + f.ns + "/gallery/456/public/cover-460.webp",
			"/" + f.ns + "/gallery/456/public/cover-460.webp",
			"/" + f.ns + "/gallery/456/private/" + sha("a"),
		} {
			t.Run(p, func(t *testing.T) { denied(t, do(t, srv, "GET", withQuery(p, "t", item), cookie(item))) })
		}
	})

	t.Run("range, conditional and HEAD", func(t *testing.T) {
		r := do(t, srv, "GET", blobA, map[string]string{"Cookie": "mt=" + item, "Range": "bytes=2-5"})
		expect(t, r, 206, bodyA[2:6])
		if r.header.Get("Content-Range") != "bytes 2-5/20" || r.header.Get("Content-Length") != "4" {
			t.Fatalf("range headers: %v", r.header)
		}
		expect(t, do(t, srv, "GET", blobA, map[string]string{"Cookie": "mt=" + item, "Range": "bytes=100-"}), 416, "")
		h := do(t, srv, "HEAD", blobA, cookie(item))
		expect(t, h, 200, "")
		if h.header.Get("Content-Length") != "20" || h.header.Get("ETag") == "" || h.header.Get("Accept-Ranges") != "bytes" {
			t.Fatalf("HEAD headers: %v", h.header)
		}
		nm := do(t, srv, "GET", blobA, map[string]string{"Cookie": "mt=" + item, "If-None-Match": h.header.Get("ETag")})
		expect(t, nm, 304, "")
		if nm.header.Get("ETag") != h.header.Get("ETag") || nm.header.Get("Cache-Control") != "private, max-age=31536000, immutable" {
			t.Fatalf("304 headers: %v", nm.header)
		}
		same(t, do(t, srv, "HEAD", blobA, nil), notFound("HEAD"))
	})

	t.Run("a denial never reaches the bucket", func(t *testing.T) {
		var calls atomic.Int64
		cfg := f.config(t)
		cfg.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return http.DefaultTransport.RoundTrip(r)
		})}
		counted := serve(t, cfg)
		denied(t, do(t, counted, "GET", blobA, nil))
		denied(t, do(t, counted, "GET", withQuery(blobA, "t", fileA, "dl", "x.mp4"), nil))
		denied(t, do(t, counted, "GET", cover, map[string]string{"Host": "other.test"}))
		if n := calls.Load(); n != 0 {
			t.Fatalf("denials made %d bucket requests", n)
		}
		expect(t, do(t, counted, "GET", blobA, cookie(item)), 200, bodyA)
		if calls.Load() != 1 {
			t.Fatal("an authorized request did not reach the bucket")
		}
	})

	t.Run("CORS", func(t *testing.T) {
		r := do(t, srv, "GET", blobA, map[string]string{"Origin": origin, "Cookie": "mt=" + item})
		expect(t, r, 200, bodyA)
		if r.header.Get("Access-Control-Allow-Origin") != origin || r.header.Get("Access-Control-Allow-Credentials") != "true" ||
			r.header.Get("Access-Control-Expose-Headers") != "Content-Length, Content-Range, Accept-Ranges, ETag, Content-Disposition" {
			t.Fatalf("CORS headers: %v", r.header)
		}
		if e := do(t, srv, "GET", cover, map[string]string{"Origin": "https://evil.test"}); e.status != 200 || e.header.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("foreign origin: %v", e.header)
		}
		p := do(t, srv, "OPTIONS", blobA, map[string]string{"Origin": origin, "Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": "range"})
		expect(t, p, 204, "")
		for name, want := range map[string]string{
			"Access-Control-Allow-Origin": origin, "Access-Control-Allow-Credentials": "true", "Access-Control-Allow-Methods": "GET, HEAD",
			"Access-Control-Allow-Headers": "Range, If-None-Match, If-Modified-Since, If-Range", "Access-Control-Max-Age": "86400",
		} {
			if got := p.header.Get(name); got != want {
				t.Errorf("preflight %s: %q, want %q", name, got, want)
			}
		}
		cors := map[string]string{"Origin": origin}
		denial := do(t, srv, "GET", blobA, cors)
		if denial.status != 404 || denial.header.Get("Access-Control-Allow-Origin") != origin {
			t.Fatalf("a credentialed denial must stay readable: %d %v", denial.status, denial.header)
		}
	})

	t.Run("methods and health", func(t *testing.T) {
		for _, m := range []string{"POST", "PUT", "DELETE"} {
			r := do(t, srv, m, cover, nil)
			if r.status != 405 || r.header.Get("Allow") != "GET, HEAD, OPTIONS" || r.header.Get("Cache-Control") != "no-store" {
				t.Fatalf("%s: %d %v", m, r.status, r.header)
			}
		}
		r := do(t, srv, "GET", "/healthz", map[string]string{"Host": "10.0.0.1:8080"})
		expect(t, r, 200, "ok\n")
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestBinary runs the built cmd/media-access with its environment config.
func TestBinary(t *testing.T) {
	f := seed(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "media-access")
	build := exec.Command("go", "build", "-o", bin, "github.com/open-rails/contentkit/cmd/media-access")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	keyFile := filepath.Join(dir, "token-key")
	if err := os.WriteFile(keyFile, []byte("k2:"+base64.StdEncoding.EncodeToString(k2.Secret)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-listen", "127.0.0.1:0", "-hosts", "media.test=accounts; 127.0.0.1="+f.ns)
	cmd.Env = []string{
		"MEDIA_ACCESS_S3_ENDPOINT=" + f.env.Config.Endpoint,
		"MEDIA_ACCESS_S3_BUCKET=" + f.env.Config.Bucket,
		"MEDIA_ACCESS_S3_ACCESS_KEY_ID=" + f.env.Config.AccessKeyID,
		"MEDIA_ACCESS_S3_SECRET_ACCESS_KEY=" + f.env.Config.SecretAccessKey,
		"MEDIA_ACCESS_TOKEN_KEY_FILE=" + keyFile,
		"MEDIA_ACCESS_TOKEN_KEY_PREVIOUS=k1:" + base64.RawURLEncoding.EncodeToString(k1.Secret),
		"MEDIA_ACCESS_CORS_ORIGINS=" + origin,
		"MEDIA_ACCESS_DEFAULTS=" + f.ns + "/gallery: cover-{w}.webp",
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited, done := make(chan error, 1), make(chan struct{})
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })

	addr := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			var line struct{ Msg, Addr string }
			if json.Unmarshal(sc.Bytes(), &line) == nil && line.Msg == "listening" {
				addr <- line.Addr
			}
			t.Log(sc.Text())
		}
		exited <- cmd.Wait()
		close(done)
	}()
	var base string
	select {
	case a := <-addr:
		base = "http://" + a
	case err := <-exited:
		t.Fatalf("exited before listening: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("no listening line")
	}

	exp := token.Expiry(time.Now(), time.Hour, 0)
	blob := f.path("gallery/456/private/" + sha("a"))
	for path, want := range map[string]int{
		"/healthz": 200,
		f.path("gallery/456/public/cover-460.webp"):                                                               200,
		f.path("gallery/789/public/cover-460.webp"):                                                               200,
		withQuery(blob, "t", ring(t, k2, nil).Sign(token.ItemScope(f.ns, "gallery", "456"), exp)):                 200,
		withQuery(blob, "t", ring(t, k1, nil).Sign(token.FileScope(f.key("gallery/456/private/"+sha("a"))), exp)): 200,
		blob:                                404,
		f.path("gallery/456/manifest.json"): 404,
	} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: %d %q, want %d", path, resp.StatusCode, b, want)
		}
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no graceful shutdown")
	}
}
