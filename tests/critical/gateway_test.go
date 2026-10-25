package critical_test

import (
	"bytes"
	"crypto/sha256"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/media/gateway"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
)

// Gateway fixtures are real objects and public signing APIs. Each refused
// target exists, so a 404 cannot pass merely because a fixture was absent.
func TestGatewayCapabilities(t *testing.T) {
	m := newMediaStorage(t, ck.Scope{TenantID: ck.NewTenantID(), AppID: ck.NewAppID()})
	ring, err := token.NewRing(m.key, nil)
	must(t, err)
	scope := m.gatewayConfig.Routes[0].Scope
	folder := scope.Prefix("gallery", uuid.NewString(), uuid.NewString())
	data := []byte("exact private bytes: 0123456789")
	sum := sha256.Sum256(data)
	first := folder + "private/" + layout.BlobName(sum[:], uuid.NewString())
	second := folder + "private/" + layout.BlobName(sum[:], uuid.NewString())
	download := folder + "download/" + layout.BlobName(sum[:], uuid.NewString())
	public := folder + "public/cover.webp"
	seed := func(t *testing.T, key string) {
		t.Helper()
		_, err := m.management.PutObject(t.Context(), &awss3.PutObjectInput{Bucket: &m.bucket, Key: &key, Body: bytes.NewReader(data), ContentType: aws.String("application/octet-stream")})
		must(t, err)
	}
	for _, key := range []string{first, second, download, public, folder + layout.ManifestName, folder + "temp/u-" + uuid.NewString()} {
		seed(t, key)
	}
	grant := func(key string) string { return ring.Sign(token.PrivateFileScope(key), time.Now().Add(time.Minute)) }
	request := func(t *testing.T, base, method, key, tok, host string, headers map[string]string) (*http.Response, []byte) {
		t.Helper()
		address := base + "/v1/" + key
		if tok != "" {
			address += "?t=" + url.QueryEscape(tok)
		}
		req, err := http.NewRequestWithContext(t.Context(), method, address, nil)
		must(t, err)
		if host != "" {
			req.Host = host
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		must(t, err)
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		must(t, err)
		return res, body
	}
	deny := func(t *testing.T, key, tok, host string) {
		t.Helper()
		res, body := request(t, m.gateway.URL, "GET", key, tok, host, nil)
		if res.StatusCode != 404 || res.Header.Get("Cache-Control") != "no-store" || bytes.Contains(body, data) {
			t.Fatalf("denial leaked or cached: status=%d headers=%v body=%q", res.StatusCode, res.Header, body)
		}
	}
	control := func(t *testing.T, key, tok string) {
		t.Helper()
		res, body := request(t, m.gateway.URL, "GET", key, tok, "", nil)
		if res.StatusCode != 200 || !bytes.Equal(body, data) {
			t.Fatalf("positive object control: status=%d body=%q", res.StatusCode, body)
		}
	}
	t.Run("CK-J13-S01_exact_file_grant_does_not_open_sibling_or_another_item", func(t *testing.T) {
		control(t, first, grant(first))
		control(t, second, grant(second))
		deny(t, second, grant(first), "")
		other := scope.Prefix("gallery", uuid.NewString(), uuid.NewString()) + "private/" + layout.BlobName(sum[:], uuid.NewString())
		seed(t, other)
		control(t, other, grant(other))
		deny(t, other, grant(first), "")
	})
	t.Run("CK-J13-S02_item_token_cannot_open_download_area", func(t *testing.T) {
		item := ring.Sign(strings.TrimSuffix(folder, "/"), time.Now().Add(time.Minute))
		control(t, first, item)
		control(t, second, item)
		control(t, download, ring.Sign(token.DownloadScope(download), time.Now().Add(time.Minute)))
		deny(t, download, item, "")
		deny(t, download, grant(first), "")
	})
	t.Run("CK-J13-S03_expiry_and_key_rotation", func(t *testing.T) {
		control(t, first, grant(first))
		deny(t, first, ring.Sign(token.PrivateFileScope(first), time.Now().Truncate(time.Second)), "")
		newKey := token.Key{ID: "rotated", Secret: bytes.Repeat([]byte{41}, 32)}
		for _, overlap := range []bool{true, false} {
			t.Run(map[bool]string{true: "overlap", false: "removed"}[overlap], func(t *testing.T) {
				var previous *token.Key
				if overlap {
					previous = &m.key
				}
				rotated, err := token.NewRing(newKey, previous)
				must(t, err)
				cfg := m.gatewayConfig
				cfg.Ring = rotated
				handler, err := gateway.New(cfg)
				must(t, err)
				server := httptest.NewServer(handler)
				defer server.Close()
				res, body := request(t, server.URL, "GET", first, grant(first), "", nil)
				want := 404
				if overlap {
					want = 200
				}
				if res.StatusCode != want {
					t.Fatalf("old key: %d want %d", res.StatusCode, want)
				}
				if overlap && !bytes.Equal(body, data) {
					t.Fatal("overlap changed bytes")
				}
				res, body = request(t, server.URL, "GET", first, rotated.Sign(token.PrivateFileScope(first), time.Now().Add(time.Minute)), "", nil)
				if res.StatusCode != 200 || !bytes.Equal(body, data) {
					t.Fatal("current rotation key failed")
				}
			})
		}
	})
	t.Run("CK-J13-S04_host_scope_and_encoded_traversal_refuse_existing_objects", func(t *testing.T) {
		control(t, first, grant(first))
		deny(t, first, grant(first), "other.invalid")
		for _, foreign := range []layout.Scope{{TenantID: uuid.NewString(), AppID: scope.AppID}, {TenantID: scope.TenantID, AppID: uuid.NewString()}} {
			key := strings.Replace(first, scope.Root(), foreign.Root(), 1)
			seed(t, key)
			deny(t, key, grant(key), "")
		}
		for _, key := range []string{strings.Replace(first, "private/", "private/%2e%2e/private/", 1), strings.Replace(first, "private/", "private%2f", 1), strings.Replace(first, "private/", "%70rivate/", 1)} {
			deny(t, key, grant(first), "")
		}
	})
	t.Run("CK-J13-S05_reserved_and_erasure_fenced_objects_refuse", func(t *testing.T) {
		control(t, first, grant(first))
		for _, key := range []string{folder + layout.ManifestName, folder + "temp/u-" + uuid.NewString(), folder + "public/" + layout.ErasureFenceName} {
			seed(t, key)
			deny(t, key, ring.Sign(strings.TrimSuffix(folder, "/"), time.Now().Add(time.Minute)), "")
		}
		// The last seeded object is the durable erasure fence. Old signed
		// URLs and public renditions must now fail even while bytes remain.
		deny(t, first, grant(first), "")
		deny(t, public, "", "")
		_, err := m.management.DeleteObject(t.Context(), &awss3.DeleteObjectInput{Bucket: &m.bucket, Key: aws.String(folder + "public/" + layout.ErasureFenceName)})
		must(t, err)
		control(t, first, grant(first))
	})
	t.Run("CK-J13-S06_get_head_range_and_conditional_preserve_bytes", func(t *testing.T) {
		tok := grant(first)
		res, body := request(t, m.gateway.URL, "GET", first, tok, "", nil)
		if res.StatusCode != 200 || !bytes.Equal(body, data) || res.Header.Get("ETag") == "" {
			t.Fatal("GET bytes or ETag missing")
		}
		etag := res.Header.Get("ETag")
		res, body = request(t, m.gateway.URL, "HEAD", first, tok, "", nil)
		if res.StatusCode != 200 || len(body) != 0 || res.ContentLength != int64(len(data)) {
			t.Fatalf("HEAD: %d %d %d", res.StatusCode, len(body), res.ContentLength)
		}
		res, body = request(t, m.gateway.URL, "GET", first, tok, "", map[string]string{"Range": "bytes=2-8"})
		if res.StatusCode != 206 || !bytes.Equal(body, data[2:9]) || res.Header.Get("Content-Range") == "" {
			t.Fatalf("range: %d %q", res.StatusCode, body)
		}
		res, body = request(t, m.gateway.URL, "GET", first, tok, "", map[string]string{"If-None-Match": etag})
		if res.StatusCode != 304 || len(body) != 0 {
			t.Fatalf("conditional: %d %q", res.StatusCode, body)
		}
		res, _ = request(t, m.gateway.URL, "HEAD", first, "", "", nil)
		if res.StatusCode != 404 {
			t.Fatalf("unauthorized HEAD: %d", res.StatusCode)
		}
	})
	t.Run("CK-J13-S08_cors_cache_and_download_headers", func(t *testing.T) {
		address := m.gateway.URL + "/v1/" + download + "?t=" + url.QueryEscape(ring.Sign(token.DownloadScope(download), time.Now().Add(time.Minute))) + "&dl=" + url.QueryEscape("report \"quoted\".txt")
		downloaded, err := http.Get(address)
		must(t, err)
		_, err = io.Copy(io.Discard, downloaded.Body)
		downloaded.Body.Close()
		must(t, err)
		if downloaded.StatusCode != 200 || downloaded.Header.Get("Content-Disposition") != "attachment" || downloaded.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("download policy: %d %v", downloaded.StatusCode, downloaded.Header)
		}
		pngBytes := imageBytes(t)
		pngSum := sha256.Sum256(pngBytes)
		pngKey := folder + "download/" + layout.BlobName(pngSum[:], uuid.NewString())
		_, err = m.management.PutObject(t.Context(), &awss3.PutObjectInput{Bucket: &m.bucket, Key: &pngKey, Body: bytes.NewReader(pngBytes), ContentType: aws.String("image/png")})
		must(t, err)
		address = m.gateway.URL + "/v1/" + pngKey + "?t=" + url.QueryEscape(ring.Sign(token.DownloadScope(pngKey), time.Now().Add(time.Minute))) + "&dl=report.png"
		downloaded, err = http.Get(address)
		must(t, err)
		got, err := io.ReadAll(downloaded.Body)
		downloaded.Body.Close()
		must(t, err)
		disposition, parameters, err := mime.ParseMediaType(downloaded.Header.Get("Content-Disposition"))
		must(t, err)
		if downloaded.StatusCode != 200 || disposition != "attachment" || parameters["filename"] != "report.png" || !bytes.Equal(got, pngBytes) {
			t.Fatalf("typed download did not preserve allowed filename and bytes: %d %v", downloaded.StatusCode, downloaded.Header)
		}
		res, _ := request(t, m.gateway.URL, "GET", first, grant(first), "", map[string]string{"Origin": "https://critical.invalid"})
		if res.Header.Get("Access-Control-Allow-Origin") != "https://critical.invalid" || res.Header.Get("Access-Control-Allow-Credentials") != "true" || !strings.HasPrefix(res.Header.Get("Cache-Control"), "private,") {
			t.Fatalf("private policy: %v", res.Header)
		}
		res, _ = request(t, m.gateway.URL, "GET", public, "", "", map[string]string{"Origin": "https://evil.invalid"})
		if res.StatusCode != 200 || res.Header.Get("Access-Control-Allow-Origin") != "" || !strings.HasPrefix(res.Header.Get("Cache-Control"), "public,") {
			t.Fatalf("public policy: %v", res.Header)
		}
		res, _ = request(t, m.gateway.URL, "OPTIONS", first, "", "", map[string]string{"Origin": "https://critical.invalid", "Access-Control-Request-Method": "GET"})
		if res.StatusCode != 204 || res.Header.Get("Access-Control-Allow-Origin") != "https://critical.invalid" {
			t.Fatalf("preflight: %d %v", res.StatusCode, res.Header)
		}
	})
}
