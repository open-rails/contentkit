package critical_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/media/worker"
	"github.com/open-rails/contentkit/media"
)

// This journey uses the shipped admin components and a real ZIP, HTTP transport,
// AuthKit identity, S3 boundary and processor. Holding the ninth original PUT
// leaves a real durable partial result; no progress snapshot is fabricated.
func TestMediaSSEUpload(t *testing.T) {
	buildDirectory := ".media-sse-upload-dist"
	if _, err := os.Stat(buildDirectory + "/media-sse-upload.html"); err != nil {
		t.Fatal("build the shipped UI and media-sse-upload harness before this selected test: pnpm --dir ../../sdk/ui build; pnpm --dir ../../sdk/ui exec vite build --config ../../tests/critical/browser/media-sse-upload.config.mjs")
	}
	// Real private media uses same-origin delivery here: IP-only loopback
	// origins on different ports do not satisfy the gateway's same-site CORP.
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer func() { server.CloseClientConnections(); server.Close() }()
	m := newResilientMedia(t, func(c *ck.Config) {
		c.Media.BaseURL = server.URL
		c.Media.Delivery.TTL = 5 * time.Minute
		gallery := &c.Media.Config.Kinds[0]
		gallery.Uploads[0].Max = 40
		gallery.Uploads = append(gallery.Uploads, media.Upload{Path: "import/{name}", Types: []string{"application/zip"}, MaxBytes: 8 << 20, Max: 2, ArchiveTo: "pages/{name}"})
		gallery.Private = append(gallery.Private, media.Private{Name: "thumb", From: "pages/{name}", To: "thumbs/{name}.webp", Image: &media.Image{Width: 32, Height: 48}})
		gallery.Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader", "thumb"}, Import: "import/{name}"}
	})
	owner, token := m.person(t, 0)
	_, outsiderToken := m.person(t, 0)
	persona, err := m.auth.Persona("app")
	must(t, err)
	authtest.GrantRole(t, m.tenant[0], m.app.group, iam.UserSubject(owner.ID), persona.OwnerRole())
	client := m.app.client(token)
	processor, err := worker.New(t.Context(), m.config)
	must(t, err)
	run, stop := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- processor.Run(run) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-stopped:
			must(t, err)
		case <-time.After(20 * time.Second):
			t.Error("media processor did not drain")
		}
	})
	originals := 0
	hold := m.wire.pause(t, func(r *http.Request) bool {
		if r.Method != http.MethodPut || !strings.HasPrefix(r.Header.Get("Content-Type"), "image/png") {
			return false
		}
		originals++
		return originals == 9
	})
	archive := mediaSSEArchive(t)
	archivePath := filepath.Join(t.TempDir(), "Progress_25_pages.zip")
	must(t, os.WriteFile(archivePath, archive, 0600))

	observed := make(chan media.RefBody, 1)
	checked := make(chan struct{})
	base, err := url.Parse(m.app.server.URL)
	must(t, err)
	mediaBase, err := url.Parse(m.storage.gateway.URL)
	must(t, err)
	mux.Handle("/v1/", httputil.NewSingleHostReverseProxy(mediaBase))
	proxy := httputil.NewSingleHostReverseProxy(base)
	proxy.FlushInterval = -1
	mux.Handle("/content/", proxy)
	mux.HandleFunc("/fixture/short-token", func(out http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			out.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		short, err := mediaShortCredential(r.Context(), m.world, token, 5*time.Second)
		if err != nil {
			http.Error(out, "sign test credential", http.StatusInternalServerError)
			return
		}
		out.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(out).Encode(map[string]string{"token": short})
	})
	mux.HandleFunc("/fixture/ref", func(out http.ResponseWriter, r *http.Request) {
		var ref media.RefBody
		if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&ref) != nil || ref.Kind != "gallery" || ref.ID == "" || ref.Version == "" {
			http.Error(out, "invalid fixture ref", 400)
			return
		}
		select {
		case observed <- ref:
			out.WriteHeader(204)
		default:
			http.Error(out, "already observed", 409)
		}
	})
	mux.HandleFunc("/fixture/release", func(out http.ResponseWriter, r *http.Request) {
		select {
		case <-checked:
			hold.unblock()
			out.WriteHeader(204)
		case <-r.Context().Done():
		}
	})
	mux.Handle("/", http.FileServer(http.Dir(buildDirectory)))
	results, err := filepath.Abs("results/media-sse-upload")
	must(t, err)
	must(t, os.MkdirAll(results, 0755))
	fixture, err := json.Marshal(map[string]any{"url": server.URL, "token": token, "viewer": owner.ID, "archive": archivePath, "bytes": len(archive), "results": results})
	must(t, err)
	browserContext, stopBrowser := context.WithTimeout(t.Context(), 150*time.Second)
	defer stopBrowser()
	command := exec.CommandContext(browserContext, "node", "browser/media-sse-upload.mjs")
	command.Env = append(os.Environ(), "CONTENTKIT_MEDIA_SSE_FIXTURE="+string(fixture))
	command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
	command.WaitDelay = 5 * time.Second
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	must(t, command.Start())
	browserDone := make(chan error, 1)
	go func() { browserDone <- command.Wait() }()
	var ref media.RefBody
	select {
	case ref = <-observed:
	case err := <-browserDone:
		t.Fatalf("browser exited before upload: %v\n%s", err, output.String())
	case <-browserContext.Done():
		t.Fatalf("browser never created its draft: %v", browserContext.Err())
	}
	select {
	case <-hold.arrived:
	case err := <-browserDone:
		t.Fatalf("browser exited before partial extraction: %v\n%s", err, output.String())
	case <-browserContext.Done():
		t.Fatal("real ZIP never reached its ninth page")
	}
	first := watchMediaSnapshot(t, client, ref, "")
	if first.Read.Status.Archives.Pages == nil || first.Read.Status.Archives.Pages.Completed != 8 || first.Read.Status.Archives.Pages.Total != 25 || first.Read.Status.State != media.StateProcessing {
		t.Fatalf("partial SSE disagrees with real extraction: %+v", first.Read.Status)
	}
	reconnected := watchMediaSnapshot(t, client, ref, first.Cursor)
	if reconnected.Read.Status.Archives.Pages.Completed != 8 {
		t.Fatalf("reconnect lost durable progress: %+v", reconnected.Read.Status)
	}
	for name, credential := range map[string]string{"anonymous": "", "other-account": outsiderToken} {
		t.Run("SSE_refuses_"+name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := m.app.client(credential).WatchMedia(ctx, ref, "", func(ck.MediaProgress) error {
				t.Error("unauthorized stream disclosed a snapshot")
				return errors.New("unexpected snapshot")
			})
			if credential == "" {
				requireError(t, err, 401, "unauthorized")
			} else {
				requireError(t, err, 404, "not_found")
			}
		})
	}
	// A signed thumbnail is readable over the actual gateway, independently
	// of the browser's rendering assertions below.
	waitForWithin(t, 15*time.Second, func() bool {
		read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
		must(t, err)
		for _, file := range read.Files {
			if file.Path == "thumbs/001.webp" && file.URL != "" {
				assertImage(t, file.URL, 32, 48)
				return true
			}
		}
		return false
	})
	close(checked)
	select {
	case err := <-browserDone:
		if err != nil {
			t.Fatalf("real shared admin journey: %v\n%s", err, output.String())
		}
	case <-browserContext.Done():
		t.Fatalf("real admin journey timed out: %v", browserContext.Err())
	}
	var receipt struct {
		Status string `json:"status"`
		Pages  int    `json:"pages"`
	}
	must(t, json.Unmarshal(bytes.TrimSpace(output.Bytes()), &receipt))
	if receipt.Status != "passed" || receipt.Pages != 25 {
		t.Fatalf("browser did not complete expected journey: %s", output.String())
	}
	final := watchMediaSnapshot(t, client, ref, first.Cursor)
	if final.Read.Status.State != media.StateReady || final.Read.Status.Archives.Pages.Completed != 25 {
		t.Fatalf("completed stream disagrees with browser: %+v", final.Read.Status)
	}
	item, err := client.GetContent(t.Context(), ref.ID, ck.GetContentQuery{})
	must(t, err)
	if item.State != catalog.StateDraft {
		t.Fatalf("upload unexpectedly published: %s", item.State)
	}
	t.Run("SSE_protocol_retention_gap_resynchronizes_current_state", func(t *testing.T) {
		before := m.clock.now()
		m.clock.set(before.Add(91 * 24 * time.Hour))
		defer m.clock.set(before)
		resync := watchMediaSnapshot(t, client, ref, first.Cursor, "resync")
		if resync.Read.Status.State != media.StateReady || resync.Read.Status.Archives.Pages.Completed != 25 {
			t.Fatalf("retention resync lost current media: %+v", resync.Read.Status)
		}
	})
	t.Run("SSE_protocol_callback_backpressure_and_cancellation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		var callbacks atomic.Int32
		stop := errors.New("observed later processing event")
		go func() {
			done <- client.WatchMedia(ctx, ref, final.Cursor, func(update ck.MediaProgress) error {
				if update.Read == nil {
					return nil
				}
				if callbacks.Add(1) == 1 {
					close(entered)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				if update.Event != "progress" {
					return fmt.Errorf("later observation=%s", update.Event)
				}
				return stop
			})
		}()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("stream produced no initial snapshot")
		}
		_, err := client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpRename, Path: "pages/001.png", To: "pages/001-renamed.png"}}})
		must(t, err)
		// Confirm the real producer has committed a later fact while the first
		// callback remains held. This read neither ACKs nor manufactures events.
		waitForWithin(t, 10*time.Second, func() bool {
			page, err := m.app.engine.Client().ListEvents(t.Context(), catalog.EventQuery{After: final.Cursor, Types: []string{media.EventProcessingProgressed, media.EventProcessingCompleted}})
			must(t, err)
			return len(page.Events) > 0
		})
		if callbacks.Load() != 1 {
			t.Fatal("progress callbacks ran concurrently")
		}
		close(release)
		select {
		case err := <-done:
			if !errors.Is(err, stop) || callbacks.Load() != 2 {
				t.Fatalf("serial progress callback result: %v, count%d", err, callbacks.Load())
			}
		case <-ctx.Done():
			t.Fatal("stream did not resume after callback release")
		}
		cancelled, stopCancelled := context.WithCancel(t.Context())
		defer stopCancelled()
		err = client.WatchMedia(cancelled, ref, "", func(update ck.MediaProgress) error {
			if update.Read != nil {
				stopCancelled()
			}
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stream cancellation=%v", err)
		}
	})
	t.Run("SSE_protocol_expired_credential_ends_an_open_stream", func(t *testing.T) {
		short, err := mediaShortCredential(t.Context(), m.world, token, 5*time.Second)
		must(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		observations := 0
		err = m.app.client(short).WatchMedia(ctx, ref, "", func(update ck.MediaProgress) error {
			if update.Read != nil {
				observations++
			}
			return nil
		})
		var refusal *ck.Error
		if observations == 0 || !errors.As(err, &refusal) || refusal.Code != "unauthorized" {
			t.Fatalf("live credential expiry: snapshots=%d error=%v", observations, err)
		}
	})
	t.Run("SSE_protocol_revoked_session_ends_an_open_stream", func(t *testing.T) {
		observer, observerToken := m.person(t, 0)
		authtest.GrantRole(t, m.tenant[0], m.app.group, iam.UserSubject(observer.ID), m.writer)
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		first := make(chan struct{}, 1)
		done := make(chan error, 1)
		var callbacks atomic.Int32
		go func() {
			done <- m.app.client(observerToken).WatchMedia(ctx, ref, "", func(update ck.MediaProgress) error {
				if update.Read != nil && callbacks.Add(1) == 1 {
					first <- struct{}{}
				}
				return nil
			})
		}()
		select {
		case <-first:
		case <-ctx.Done():
			t.Fatal("authorized observer received no snapshot")
		}
		claims, err := m.tenant[0].Verify(t.Context(), observerToken)
		must(t, err)
		identity := authtest.Identity(t, m.tenant[0], observerToken)
		must(t, m.tenant[0].RevokeSession(t.Context(), identity, observer.ID, claims.SessionID))
		select {
		case err := <-done:
			var refusal *ck.Error
			if !errors.As(err, &refusal) || refusal.Code != "unauthorized" {
				t.Fatalf("revoked live stream=%v", err)
			}
		case <-ctx.Done():
			t.Fatal("revoked stream remained open")
		}
	})
}

func watchMediaSnapshot(t *testing.T, client *ck.Client, ref media.RefBody, cursor string, event ...string) ck.MediaProgress {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stop := errors.New("observed current snapshot")
	var observed ck.MediaProgress
	err := client.WatchMedia(ctx, ref, cursor, func(progress ck.MediaProgress) error {
		if progress.Read == nil {
			return nil
		}
		observed = progress
		return stop
	})
	expected := "snapshot"
	if len(event) != 0 {
		expected = event[0]
	}
	if !errors.Is(err, stop) || observed.Event != expected || observed.Cursor == "" || observed.Read.Status == nil || len(observed.Read.Files) != 0 || observed.Read.Cookie != nil {
		t.Fatalf("authorized status snapshot/cursor without capabilities: %+v, %v", observed, err)
	}
	encoded, err := json.Marshal(observed.Read)
	must(t, err)
	if len(encoded) > 1<<20 {
		t.Fatalf("status frame exceeds its public bound: %d", len(encoded))
	}
	return observed
}

func mediaSSEArchive(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	archive := zip.NewWriter(&body)
	for page := 1; page <= 25; page++ {
		pixels := image.NewNRGBA(image.Rect(0, 0, 128, 192))
		noise := rand.New(rand.NewSource(int64(page)))
		for i := 0; i < len(pixels.Pix); i += 4 {
			pixels.Pix[i], pixels.Pix[i+1], pixels.Pix[i+2], pixels.Pix[i+3] = uint8(noise.Intn(256)), uint8(noise.Intn(256)), uint8(noise.Intn(256)), 255
		}
		file, err := archive.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("%03d.png", page), Method: zip.Store})
		must(t, err)
		encoder := png.Encoder{CompressionLevel: png.NoCompression}
		must(t, encoder.Encode(file, pixels))
	}
	must(t, archive.Close())
	return body.Bytes()
}

// This is an actual token for the same real session, signed by the fixture's
// registered AuthKit key. Only its bounded expiry changes.
func mediaShortCredential(ctx context.Context, world *world, token string, ttl time.Duration) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("fixture access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	var claims map[string]any
	if err = json.Unmarshal(payload, &claims); err != nil {
		return "", err
	}
	claims["exp"] = time.Now().Add(ttl).Unix()
	payload, err = json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := parts[0] + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature, err := world.signer.Sign(ctx, []byte(unsigned))
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
