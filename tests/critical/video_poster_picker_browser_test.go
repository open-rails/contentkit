package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
)

// The shipped poster picker in a real browser against a video the actual
// worker encoded: the strip is the seek sprite, the preview and crop source
// the browser's own decode of the HLS rendition, and saves are frame ops the
// worker grabs, from the item's video or another item's.
func TestVideoPosterPickerBrowser(t *testing.T) {
	if _, err := os.Stat(".browser-dist/video-poster-picker.html"); err != nil {
		t.Fatal("build the shipped UI and critical browser harness before this selected test: pnpm --dir ../../sdk/ui build; pnpm --dir ../../sdk/ui exec vite build --config ../../tests/critical/browser/vite.config.mjs")
	}
	// Same-origin delivery: loopback origins on different ports do not
	// satisfy the gateway's same-site CORP for the sprite sheet.
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer func() { server.CloseClientConnections(); server.Close() }()
	f := newTinyHLSConfiguredFixture(t, false, func(c *ck.Config) {
		withPosters(c)
		c.Media.BaseURL = server.URL
	})
	editor, token := f.world.person(t, 0)
	authtest.GrantRole(t, f.world.tenant[0], f.app.group, iam.UserSubject(editor.ID), f.world.writer)
	admin := f.app.engine.Client()
	item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "clip", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Poster from another clip"}}}}})
	must(t, err)
	other := media.RefBody{Kind: "clip", ID: item.ID, Version: item.Versions[0].ID}
	app, err := url.Parse(f.app.server.URL)
	must(t, err)
	gateway, err := url.Parse(f.storage.gateway.URL)
	must(t, err)
	mux.Handle("/v1/", httputil.NewSingleHostReverseProxy(gateway))
	api := httputil.NewSingleHostReverseProxy(app)
	api.FlushInterval = -1 // waitFor watches the processing stream
	mux.Handle("/content/", api)
	mux.Handle("/", http.FileServer(http.Dir(".browser-dist")))
	results, err := filepath.Abs("results/video-poster-picker")
	must(t, err)
	must(t, os.MkdirAll(results, 0o755))
	fixture, err := json.Marshal(map[string]any{"url": server.URL, "token": token, "own": f.Ref, "other": other, "results": results})
	must(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "node", "browser/video-poster-picker.mjs")
	command.Env = append(os.Environ(), "CONTENTKIT_POSTER_FIXTURE="+string(fixture))
	command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
	command.WaitDelay = 5 * time.Second
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		t.Fatalf("real poster picker: %v\n%s", err, output.String())
	}
	var receipt struct {
		Status     string `json:"status"`
		OwnShown   string `json:"ownShown"`
		OtherShown string `json:"otherShown"`
	}
	must(t, json.Unmarshal(bytes.TrimSpace(output.Bytes()), &receipt))
	if receipt.Status != "passed" {
		t.Fatalf("browser did not complete the picker journey: %s", output.String())
	}
	// The worker grabbed both from the source, as the browser chose them.
	own := awaitPoster(t, admin, f.Ref)
	if own.Frame == nil || own.Frame.T != 1.625 || own.Frame.From != nil || own.Edit == nil || own.Edit.Crop == nil || own.W != 256 || own.H != 144 {
		t.Fatalf("own poster: %+v frame %+v edit %+v", own, own.Frame, own.Edit)
	}
	if c := own.Edit.Crop; c.W >= 256 || c.X+c.W > 256 || c.Y+c.H > 144 || c.W*9 < c.H*16-16 || c.W*9 > c.H*16+16 {
		t.Fatalf("crop is not a 16:9 region of the 256x144 source: %+v", c)
	}
	grabbed := awaitPoster(t, admin, other)
	want := media.CopyFrom{Kind: "clip", ID: f.Ref.ID, Version: f.Ref.Version, Path: "source.mp4"}
	if grabbed.Frame == nil || grabbed.Frame.From == nil || *grabbed.Frame.From != want || grabbed.Frame.T != 0.5 || grabbed.W != 256 {
		t.Fatalf("poster from another clip: %+v frame %+v", grabbed, grabbed.Frame)
	}
	// The preview was exact: the worker grabbed the very frame the browser
	// showed (frame 19 at 1.625 s, frame 6 at 0.5 s), not a neighbour.
	requireSameFrame(t, receipt.OwnShown, own, f.Source, 19)
	requireSameFrame(t, receipt.OtherShown, grabbed, f.Source, 6)
}

// requireSameFrame compares the browser's RGB (its own colour conversion)
// with the worker's grab and the source's frames n-1 and n+1 (ffmpeg's): the
// grab, frame n, must be clearly the nearest.
func requireSameFrame(t *testing.T, shown string, poster media.FileInfo, source []byte, n int) {
	t.Helper()
	file, err := os.Open(shown)
	must(t, err)
	defer file.Close()
	browser, err := png.Decode(file)
	must(t, err)
	res, err := http.Get(poster.URL)
	must(t, err)
	defer res.Body.Close()
	worker, err := png.Decode(res.Body)
	must(t, err)
	if browser.Bounds() != worker.Bounds() {
		t.Fatalf("browser frame %v, worker frame %v", browser.Bounds(), worker.Bounds())
	}
	same := frameDistance(browser, worker)
	for _, other := range []int{n - 1, n + 1} {
		if d := frameDistance(browser, sourceFrame(t, source, other)); same*1.5 >= d {
			grabbed := strings.TrimSuffix(shown, ".png") + "-grabbed.png"
			out, err := os.Create(grabbed)
			must(t, err)
			must(t, errors.Join(png.Encode(out, worker), out.Close()))
			t.Fatalf("the browser showed (%s) frame %d rather than the grab (%s): distance %.1f, %.1f to the grab", shown, other, grabbed, d, same)
		}
	}
}
