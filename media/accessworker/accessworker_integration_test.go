package accessworker_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/accessworker"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/token"
)

var (
	k0 = token.Key{ID: "k0", Secret: bytes.Repeat([]byte{7}, 32)}
	k1 = token.Key{ID: "k1", Secret: bytes.Repeat([]byte{1}, 32)}
	k2 = token.Key{ID: "k2", Secret: bytes.Repeat([]byte{2}, 32)}
)

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return media.SHA256Name(sum[:])
}

func mustRing(t *testing.T, cur token.Key, prev *token.Key) token.Ring {
	t.Helper()
	r, err := token.NewRing(cur, prev)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type fixture struct {
	env                                     *s3test.Env
	item, blobA, blobB, other, orig, public string
	bodyA                                   string
}

func seed(t *testing.T) *fixture {
	t.Helper()
	env := s3test.Open(t)
	f := &fixture{env: env, item: env.Tenant + "/gallery/1/", bodyA: "0123456789abcdefghij"}
	f.blobA = f.item + "blobs/" + sha("a")
	f.blobB = f.item + "blobs/" + sha("b")
	f.orig = f.item + "originals/" + sha("o")
	f.public = f.item + "public/cover.webp"
	f.other = env.Tenant + "/gallery/2/blobs/" + sha("c")
	ctx := context.Background()
	for key, body := range map[string]string{
		f.blobA: f.bodyA, f.blobB: "bee", f.other: "other", f.orig: "original",
		f.public: "cover", f.item + "manifest.json": "{}", f.item + "manifests/v1.json": "{}",
	} {
		if _, err := env.Store.Put(ctx, key, strings.NewReader(body), int64(len(body)), media.PutOptions{ContentType: "image/webp"}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fixture) handler(t *testing.T, mut func(*accessworker.Config)) *httptest.Server {
	t.Helper()
	cfg := accessworker.Config{
		Endpoint: f.env.Config.Endpoint, Bucket: f.env.Config.Bucket, Region: f.env.Config.Region,
		AccessKeyID: f.env.Config.AccessKeyID, SecretAccessKey: f.env.Config.SecretAccessKey,
		Ring:    mustRing(t, k2, &k1),
		Origins: []string{"https://doujins.com"},
	}
	if mut != nil {
		mut(&cfg)
	}
	h, err := accessworker.New(cfg)
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

func do(t *testing.T, srv *httptest.Server, method, path string, hdr map[string]string) result {
	t.Helper()
	u, err := url.Parse(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	req := &http.Request{Method: method, URL: u, Header: http.Header{}, Host: u.Host}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{resp.StatusCode, resp.Header, string(b)}
}

func withToken(key, tok string, extra ...string) string {
	q := url.Values{"t": {tok}}
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	return "/" + key + "?" + q.Encode()
}

func TestAccessWorker(t *testing.T) {
	f := seed(t)
	srv := f.handler(t, nil)
	exp := token.Expiry(time.Now(), time.Hour, 0)
	cur := mustRing(t, k2, nil)
	folder := cur.Sign(f.item+"blobs/", exp)
	fileA := cur.Sign(token.FileScope(f.blobA), exp)

	expect := func(t *testing.T, r result, status int, body string) {
		t.Helper()
		if r.status != status || (body != "" && r.body != body) {
			t.Fatalf("got %d %q; want %d %q", r.status, r.body, status, body)
		}
	}

	t.Run("public passthrough", func(t *testing.T) {
		r := do(t, srv, "GET", "/"+f.public, nil)
		expect(t, r, 200, "cover")
		if r.header.Get("Cache-Control") != "public, no-cache" || r.header.Get("ETag") == "" {
			t.Fatalf("public headers: %v", r.header)
		}
		nm := do(t, srv, "GET", "/"+f.public, map[string]string{"If-None-Match": r.header.Get("ETag")})
		expect(t, nm, 304, "")
		if nm.header.Get("ETag") != r.header.Get("ETag") || nm.body != "" {
			t.Fatalf("304: %v %q", nm.header, nm.body)
		}
	})

	t.Run("blob needs a token", func(t *testing.T) {
		expect(t, do(t, srv, "GET", "/"+f.blobA, nil), 403, "")
		expect(t, do(t, srv, "GET", withToken(f.blobA, "garbage"), nil), 403, "")
	})

	t.Run("URL mode per-file token", func(t *testing.T) {
		r := do(t, srv, "GET", withToken(f.blobA, fileA), nil)
		expect(t, r, 200, f.bodyA)
		if r.header.Get("Cache-Control") != "private, max-age=31536000, immutable" || r.header.Get("Content-Type") != "image/webp" ||
			r.header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("blob headers: %v", r.header)
		}
		expect(t, do(t, srv, "GET", withToken(f.blobB, fileA), nil), 403, "")
	})

	t.Run("URL mode folder token", func(t *testing.T) {
		expect(t, do(t, srv, "GET", withToken(f.blobA, folder), nil), 200, f.bodyA)
		expect(t, do(t, srv, "GET", withToken(f.blobB, folder), nil), 200, "bee")
		expect(t, do(t, srv, "GET", withToken(f.other, folder), nil), 403, "")
		expect(t, do(t, srv, "GET", withToken(f.blobA+"/x", folder), nil), 404, "")
		expect(t, do(t, srv, "GET", withToken(f.orig, folder), nil), 404, "")
		expect(t, do(t, srv, "GET", withToken(f.item+"manifest.json", folder), nil), 404, "")
		expect(t, do(t, srv, "GET", withToken(f.item+"manifests/v1.json", folder), nil), 404, "")
	})

	t.Run("originals and manifests are never served", func(t *testing.T) {
		for _, scope := range []string{f.item, f.item + "originals/", f.orig} {
			tok := cur.Sign(scope, exp)
			expect(t, do(t, srv, "GET", withToken(f.orig, tok), nil), 404, "")
			expect(t, do(t, srv, "GET", "/"+f.orig, map[string]string{"Cookie": "mt=" + tok}), 404, "")
		}
		expect(t, do(t, srv, "GET", "/"+f.orig, nil), 404, "")
		expect(t, do(t, srv, "GET", withToken(f.blobA, cur.Sign(f.item, exp)), nil), 403, "")
	})

	t.Run("cookie mode", func(t *testing.T) {
		expect(t, do(t, srv, "GET", "/"+f.blobA, map[string]string{"Cookie": "mt=" + folder}), 200, f.bodyA)
		otherFolder := cur.Sign(f.env.Tenant+"/gallery/2/blobs/", exp)
		expect(t, do(t, srv, "GET", "/"+f.blobA, map[string]string{"Cookie": "mt=" + otherFolder}), 403, "")
		expect(t, do(t, srv, "GET", "/"+f.blobA, map[string]string{"Cookie": "mt=" + otherFolder + "; mt=" + folder}), 200, f.bodyA)
		expect(t, do(t, srv, "GET", "/"+f.blobA, map[string]string{"Cookie": "other=" + folder}), 403, "")
	})

	t.Run("expiry and key rotation", func(t *testing.T) {
		expired := cur.Sign(f.blobA, time.Now().Add(-time.Second))
		expect(t, do(t, srv, "GET", withToken(f.blobA, expired), nil), 403, "")
		expect(t, do(t, srv, "GET", "/"+f.blobA, map[string]string{"Cookie": "mt=" + expired}), 403, "")
		prev := mustRing(t, k1, nil).Sign(f.blobA, exp)
		expect(t, do(t, srv, "GET", withToken(f.blobA, prev), nil), 200, f.bodyA)
		unknown := mustRing(t, k0, nil).Sign(f.blobA, exp)
		expect(t, do(t, srv, "GET", withToken(f.blobA, unknown), nil), 403, "")
		rotated := f.handler(t, func(c *accessworker.Config) { c.Ring = mustRing(t, k0, &k2) })
		expect(t, do(t, rotated, "GET", withToken(f.blobA, fileA), nil), 200, f.bodyA)
		expect(t, do(t, rotated, "GET", withToken(f.blobA, prev), nil), 403, "")
	})

	t.Run("signed download name", func(t *testing.T) {
		name := `Title "ep" (1080p) ✓.mp4`
		dl := cur.Sign(token.DownloadScope(f.blobA, name), exp)
		r := do(t, srv, "GET", withToken(f.blobA, dl, "dl", name), nil)
		expect(t, r, 200, f.bodyA)
		if got := r.header.Get("Content-Disposition"); got != token.Attachment(name) {
			t.Fatalf("Content-Disposition %q", got)
		}
		expect(t, do(t, srv, "GET", withToken(f.blobA, dl, "dl", "Other.mp4"), nil), 403, "")
		expect(t, do(t, srv, "GET", withToken(f.blobA, dl), nil), 403, "")
		expect(t, do(t, srv, "GET", withToken(f.blobA, fileA, "dl", name), nil), 403, "")
		cookie := do(t, srv, "GET", "/"+f.blobA+"?dl="+url.QueryEscape(name), map[string]string{"Cookie": "mt=" + folder})
		expect(t, cookie, 403, "")
		if plain := do(t, srv, "GET", withToken(f.blobA, fileA), nil); plain.header.Get("Content-Disposition") != "" {
			t.Fatal("no dl, no Content-Disposition")
		}
	})

	t.Run("range, conditional and HEAD", func(t *testing.T) {
		r := do(t, srv, "GET", withToken(f.blobA, fileA), map[string]string{"Range": "bytes=2-5"})
		expect(t, r, 206, f.bodyA[2:6])
		if r.header.Get("Content-Range") != "bytes 2-5/20" || r.header.Get("Content-Length") != "4" {
			t.Fatalf("range headers: %v", r.header)
		}
		expect(t, do(t, srv, "GET", withToken(f.blobA, fileA), map[string]string{"Range": "bytes=100-"}), 416, "")
		h := do(t, srv, "HEAD", withToken(f.blobA, fileA), nil)
		expect(t, h, 200, "")
		if h.header.Get("Content-Length") != "20" || h.header.Get("ETag") == "" || h.header.Get("Accept-Ranges") != "bytes" {
			t.Fatalf("HEAD headers: %v", h.header)
		}
		nm := do(t, srv, "GET", withToken(f.blobA, fileA), map[string]string{"If-None-Match": h.header.Get("ETag")})
		expect(t, nm, 304, "")
		expect(t, do(t, srv, "HEAD", "/"+f.blobA, nil), 403, "")
	})

	t.Run("non-canonical paths", func(t *testing.T) {
		for _, p := range []string{
			f.item + "blobs/../originals/" + sha("o"),
			f.item + "blobs/%2e%2e/originals/" + sha("o"),
			f.item + "blobs/%2E%2E%2Foriginals%2F" + sha("o"),
			strings.Replace(f.blobA, "/blobs/", "//blobs/", 1),
			strings.Replace(f.blobA, "/blobs/", "/blobs%2F", 1),
			f.item + "blobs/" + strings.ToUpper(sha("a")),
			f.item + "public/../originals/" + sha("o"),
			f.item + "public/cover.png",
			"./" + f.blobA,
		} {
			r := do(t, srv, "GET", withToken(p, folder), map[string]string{"Cookie": "mt=" + folder})
			if r.status != 404 {
				t.Errorf("%s: %d", p, r.status)
			}
		}
	})

	t.Run("missing blob", func(t *testing.T) {
		missing := f.item + "blobs/" + sha("missing")
		expect(t, do(t, srv, "GET", withToken(missing, folder), nil), 404, "")
	})

	t.Run("CORS", func(t *testing.T) {
		r := do(t, srv, "GET", "/"+f.blobA, map[string]string{"Origin": "https://doujins.com", "Cookie": "mt=" + folder})
		expect(t, r, 200, f.bodyA)
		if r.header.Get("Access-Control-Allow-Origin") != "https://doujins.com" || r.header.Get("Access-Control-Allow-Credentials") != "true" ||
			!strings.Contains(r.header.Get("Access-Control-Expose-Headers"), "Content-Range") {
			t.Fatalf("CORS headers: %v", r.header)
		}
		if e := do(t, srv, "GET", "/"+f.public, map[string]string{"Origin": "https://evil.example"}); e.header.Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("foreign origin allowed")
		}
		p := do(t, srv, "OPTIONS", "/"+f.blobA, map[string]string{"Origin": "https://doujins.com", "Access-Control-Request-Method": "GET"})
		expect(t, p, 204, "")
		if !strings.Contains(p.header.Get("Access-Control-Allow-Headers"), "Range") {
			t.Fatalf("preflight: %v", p.header)
		}
	})

	t.Run("hosts, methods and health", func(t *testing.T) {
		hosted := f.handler(t, func(c *accessworker.Config) { c.Hosts = []string{"Media.Doujins.com"} })
		expect(t, do(t, hosted, "GET", "/"+f.public, map[string]string{"Host": "media.doujins.com:443"}), 200, "cover")
		expect(t, do(t, hosted, "GET", "/"+f.public, map[string]string{"Host": "evil.example"}), 421, "")
		expect(t, do(t, hosted, "GET", accessworker.HealthPath, map[string]string{"Host": "10.0.0.1:8080"}), 200, "ok\n")
		expect(t, do(t, srv, "POST", "/"+f.public, nil), 405, "")
		expect(t, do(t, srv, "DELETE", withToken(f.blobA, fileA), nil), 405, "")
	})
}

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
	cmd := exec.Command(bin, "-listen", "127.0.0.1:0", "-hosts", "media.doujins.com, 127.0.0.1")
	cmd.Env = []string{
		"MEDIA_ACCESS_S3_ENDPOINT=" + f.env.Config.Endpoint,
		"MEDIA_ACCESS_S3_BUCKET=" + f.env.Config.Bucket,
		"MEDIA_ACCESS_S3_ACCESS_KEY_ID=" + f.env.Config.AccessKeyID,
		"MEDIA_ACCESS_S3_SECRET_ACCESS_KEY=" + f.env.Config.SecretAccessKey,
		"MEDIA_ACCESS_TOKEN_KEY_FILE=" + keyFile,
		"MEDIA_ACCESS_TOKEN_KEY_PREVIOUS=k1:" + base64.RawURLEncoding.EncodeToString(k1.Secret),
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

	get := func(path string) (int, string) {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	exp := token.Expiry(time.Now(), time.Hour, 0)
	for path, want := range map[string]int{
		accessworker.HealthPath: 200,
		"/" + f.public:          200,
		withToken(f.blobA, mustRing(t, k2, nil).Sign(f.blobA, exp)): 200,
		withToken(f.blobA, mustRing(t, k1, nil).Sign(f.blobA, exp)): 200,
		withToken(f.orig, mustRing(t, k2, nil).Sign(f.orig, exp)):   404,
		"/" + f.blobA: 403,
	} {
		if got, body := get(path); got != want {
			t.Errorf("%s: %d %q, want %d", path, got, body, want)
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
