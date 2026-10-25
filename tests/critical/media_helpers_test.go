package critical_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
)

func mountEngine(t *testing.T, engine *ck.Engine) *httptest.Server {
	t.Helper()
	routes, err := engine.Routes(ck.Routes{Prefix: "/content", Groups: engine.RouteGroups()})
	must(t, err)
	mux := http.NewServeMux()
	for _, route := range routes {
		mux.Handle(route.Path, route.Handler)
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func galleryRef(t *testing.T, client *ck.Client, title string) media.RefBody {
	t.Helper()
	item, err := client.PostContents(t.Context(), catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: title}}})
	must(t, err)
	if len(item.Versions) != 1 {
		t.Fatal("new gallery must have its initial edition")
	}
	return media.RefBody{Kind: "gallery", ID: item.ID, Version: item.Versions[0].ID}
}

func awaitMediaReady(t *testing.T, client *ck.Client, ref media.RefBody) media.ReadResult {
	t.Helper()
	readySignal := errors.New("media ready")
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	err := client.WatchMedia(ctx, ref, "", func(update ck.MediaProgress) error {
		if update.Read == nil || update.Read.Status == nil {
			return nil
		}
		status := update.Read.Status
		if status.FailureCount > 0 {
			return fmt.Errorf("processing failed: %+v", status.Failures)
		}
		if status.State == media.StateReady && status.Uploads.Total > 0 {
			return readySignal
		}
		return nil
	})
	if !errors.Is(err, readySignal) {
		t.Fatalf("await durable media state: %v", err)
	}
	ready, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
	must(t, err)
	for _, file := range ready.Files {
		if file.Staged || len(file.Pending) != 0 {
			t.Fatalf("ready stream preceded settled files: %+v", file)
		}
	}
	if len(ready.Files) == 0 {
		t.Fatal("ready media contains no files")
	}
	return ready
}

func assertImage(t *testing.T, url string, width, height int) {
	t.Helper()
	res, err := http.Get(url)
	must(t, err)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("rendition status %d", res.StatusCode)
	}
	got, format, err := image.DecodeConfig(res.Body)
	must(t, err)
	if format != "webp" || got.Width != width || got.Height != height {
		t.Fatalf("rendition = %s %dx%d, want webp %dx%d", format, got.Width, got.Height, width, height)
	}
}

func digestBytes(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func transfer(t *testing.T, plan media.RequestReply, data []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), plan.Method, plan.URL, bytes.NewReader(data))
	must(t, err)
	for key, value := range plan.Headers {
		req.Header.Set(key, value)
	}
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	must(t, err)
	return res.StatusCode, body
}

func uploadPlan(t *testing.T, client *ck.Client, ref media.RefBody, path, kind string, data []byte) media.PresignReply {
	t.Helper()
	plan, err := client.PostMediaUploadPresign(t.Context(), media.PresignBody{Ref: ref, Path: path, Type: kind, Size: int64(len(data)), SHA256: digestBytes(data)})
	must(t, err)
	return plan
}

func stageFile(t *testing.T, client *ck.Client, ref media.RefBody, path string, data []byte) media.Op {
	t.Helper()
	plan := uploadPlan(t, client, ref, path, "image/png", data)
	if !plan.Exists {
		if plan.Put == nil {
			t.Fatal("small image did not receive a PUT plan")
		}
		if code, body := transfer(t, *plan.Put, data); code != http.StatusOK {
			t.Fatalf("S3 PUT: %d %s", code, body)
		}
	}
	return media.Op{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}
}

func requireMediaPaths(t *testing.T, read media.ReadResult, expected ...string) {
	t.Helper()
	paths := map[string]bool{}
	for _, file := range read.Files {
		if paths[file.Path] {
			t.Fatalf("duplicate media path %s", file.Path)
		}
		paths[file.Path] = true
	}
	for _, path := range expected {
		if !paths[path] {
			t.Fatalf("missing media path %s in %+v", path, read.Files)
		}
	}
	if len(paths) != len(expected) {
		t.Fatalf("unexpected adopted media paths: %+v", paths)
	}
}
