package contentkit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/media"
)

type mediaTransport func(*http.Request) (*http.Response, error)

func (f mediaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mediaJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func TestIngestMediaBatchesSignaturesAndUsesPartNumbers(t *testing.T) {
	// Tiny fixture limits exercise the >100-part contract without allocating
	// gigabytes. The final part is shorter, making positional joins observable.
	data := make([]byte, 101*8+3)
	for i := range data {
		data[i] = byte(i % 251)
	}
	var server *httptest.Server
	var mu sync.Mutex
	var batches []int
	var puts int
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/v1/media/upload/presign":
			mediaJSON(t, w, media.PresignReply{Path: "import/archive.zip", Blob: "staged", Multipart: &media.MultipartReply{
				Ticket: "ticket", MinPartSize: 4, MaxPartSize: 8, MaxParts: 200,
			}})
		case r.URL.Path == "/v1/media/upload/parts":
			var request media.PartsBody
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			batches = append(batches, len(request.Parts))
			if len(request.Parts) > 100 {
				http.Error(w, "too many parts", http.StatusBadRequest)
				return
			}
			out := media.PartsReply{}
			// Server order is not required to match request order.
			for i := len(request.Parts) - 1; i >= 0; i-- {
				p := request.Parts[i]
				out.Parts = append(out.Parts, media.PartReply{Number: p.Number, Request: &media.RequestReply{
					Method: http.MethodPut, URL: server.URL + "/part/" + strconv.Itoa(int(p.Number)),
				}})
			}
			mediaJSON(t, w, out)
		case strings.HasPrefix(r.URL.Path, "/part/"):
			n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/part/"))
			body, _ := io.ReadAll(r.Body)
			offset := (n - 1) * 8
			if !bytes.Equal(body, data[offset:min(offset+8, len(data))]) {
				t.Errorf("wrong bytes for part %d: %x", n, body)
			}
			puts++
		case r.URL.Path == "/v1/media/upload/complete":
			mediaJSON(t, w, media.CompleteReply{Blob: "staged", Size: int64(len(data)), Type: "application/zip"})
		case r.URL.Path == "/v1/media/upload/commit":
			mediaJSON(t, w, media.CommitReply{})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	_, err := ck.NewClient(server.URL, nil).IngestMedia(context.Background(), nil, media.RefBody{Kind: "gallery", ID: "id", Version: "version"},
		"550e8400-e29b-41d4-a716-446655440003", []ck.MediaFile{{Path: "import/archive.zip", Type: "application/zip", Body: bytes.NewReader(data)}})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 2 || batches[0] != 100 || batches[1] != 2 || puts != 102 {
		t.Fatalf("signature batches=%v, uploaded parts=%d", batches, puts)
	}
}

func TestIngestMediaRejectsInvalidSignatureSetsBeforeUploading(t *testing.T) {
	for _, mode := range []string{"missing", "duplicate", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			var server *httptest.Server
			var puts int
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/media/upload/presign":
					mediaJSON(t, w, media.PresignReply{Multipart: &media.MultipartReply{Ticket: "ticket", MinPartSize: 4, MaxPartSize: 8, MaxParts: 10}})
				case "/v1/media/upload/parts":
					parts := []media.PartReply{{Number: 1, Request: &media.RequestReply{Method: http.MethodPut, URL: server.URL + "/upload"}}}
					if mode == "duplicate" {
						parts = append(parts, parts[0])
					}
					if mode == "unknown" {
						parts = append(parts, media.PartReply{Number: 99, Request: parts[0].Request})
					}
					mediaJSON(t, w, media.PartsReply{Parts: parts})
				case "/upload":
					puts++
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			_, err := ck.NewClient(server.URL, nil).IngestMedia(context.Background(), nil, media.RefBody{Kind: "gallery", ID: "id"},
				"550e8400-e29b-41d4-a716-446655440004", []ck.MediaFile{{Path: "archive.zip", Type: "application/zip", Body: bytes.NewReader([]byte("123456789"))}})
			if err == nil || puts != 0 {
				t.Fatalf("invalid signatures: error=%v, uploads=%d", err, puts)
			}
		})
	}
}

func TestIngestMediaOnlyUploadsAndCommitsWithIsolatedStorageCredentials(t *testing.T) {
	data := []byte("archive bytes")
	ref := media.RefBody{Kind: "gallery", ID: "content", Version: "edition"}
	const commitID = "550e8400-e29b-41d4-a716-446655440020"
	var stored []byte
	var mu sync.Mutex
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPut || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("storage received API credentials or wrong method: %s %v", r.Method, r.Header)
		}
		if r.Header.Get("X-Signed-Upload") != "expected" {
			t.Error("storage request lost signed headers")
		}
		stored, _ = io.ReadAll(r.Body)
	}))
	defer storage.Close()
	jar, _ := cookiejar.New(nil)
	storageURL, _ := url.Parse(storage.URL)
	jar.SetCookies(storageURL, []*http.Cookie{{Name: "session", Value: "must-not-reach-storage"}})
	var paths []string
	var commit media.CommitBody
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer private-contentkit-key" {
			t.Error("API request lost authentication")
		}
		switch r.URL.Path {
		case "/v1/media/upload/presign":
			mediaJSON(t, w, media.PresignReply{Path: "import/archive.zip", Blob: "staged-archive", Put: &media.RequestReply{
				Method: http.MethodPut, URL: storage.URL + "/upload", Headers: map[string]string{"X-Signed-Upload": "expected"},
			}})
		case "/v1/media/upload/commit":
			if err := json.NewDecoder(r.Body).Decode(&commit); err != nil {
				t.Error(err)
			}
			mediaJSON(t, w, media.CommitReply{Files: []media.FileInfo{
				{Path: "import/archive.zip", Type: "application/zip", Upload: true, Pending: []string{"import"}},
			}})
		default:
			t.Errorf("unexpected metadata, publication or status request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()
	client := ck.NewClient(api.URL, &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer private-contentkit-key")
		return http.DefaultTransport.RoundTrip(r)
	})})
	out, err := client.IngestMedia(t.Context(), &http.Client{Jar: jar}, ref, commitID,
		[]ck.MediaFile{{Path: "import/archive.zip", Type: "application/zip", Body: bytes.NewReader(data)}})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !bytes.Equal(stored, data) || len(out.Files) != 1 || len(out.Files[0].Pending) != 1 {
		t.Fatalf("upload or attached pages lost: stored=%q reply=%+v", stored, out)
	}
	if len(paths) != 2 || paths[0] != "POST /v1/media/upload/presign" || paths[1] != "POST /v1/media/upload/commit" {
		t.Fatalf("API flow = %v", paths)
	}
	if commit.Ref != ref || commit.OperationID != commitID || len(commit.Ops) != 1 || commit.Ops[0].Blob != "staged-archive" {
		t.Fatalf("commit changed explicit input: %+v", commit)
	}
	if len(jar.Cookies(storageURL)) != 1 {
		t.Fatal("storage isolation mutated the caller's cookie jar")
	}
}

func TestIngestMediaDoesNotFollowPresignedStorageRedirect(t *testing.T) {
	followed := false
	var mu sync.Mutex
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		followed = true
		mu.Unlock()
	}))
	defer target.Close()
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer storage.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/media/upload/presign" {
			t.Errorf("commit sent after failed storage upload: %s", r.URL.Path)
		}
		mediaJSON(t, w, media.PresignReply{Path: "import/archive.zip", Put: &media.RequestReply{Method: http.MethodPut, URL: storage.URL}})
	}))
	defer api.Close()
	customRedirect := false
	storageClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { customRedirect = true; return nil }}
	_, err := ck.NewClient(api.URL, nil).IngestMedia(t.Context(), storageClient,
		media.RefBody{Kind: "gallery", ID: "content", Version: "edition"}, "550e8400-e29b-41d4-a716-446655440021",
		[]ck.MediaFile{{Path: "import/archive.zip", Type: "application/zip", Body: bytes.NewReader([]byte("archive"))}})
	var failure *ck.Error
	mu.Lock()
	defer mu.Unlock()
	if !errors.As(err, &failure) || failure.Status != http.StatusTemporaryRedirect || followed || customRedirect {
		t.Fatalf("presigned redirect escaped its target: err=%v target=%v redirectHook=%v", err, followed, customRedirect)
	}
}

func TestIngestMediaRetainsExactCommitAfterLostResponse(t *testing.T) {
	const id = "550e8400-e29b-41d4-a716-446655440022"
	meta := map[string]any{"page": map[string]any{"title": "original", "number": int64(1<<54 + 3)}}
	var sent []byte
	var commits, presigns int
	client := ck.NewClient("https://catalog.test", &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		var body []byte
		switch r.URL.Path {
		case "/v1/media/upload/presign":
			presigns++
			body, _ = json.Marshal(media.PresignReply{Path: "import/archive.zip", Blob: "existing-archive", Exists: true})
		case "/v1/media/upload/commit":
			commits++
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if commits == 1 {
				sent = append([]byte{}, raw...)
				// A caller editing its own metadata after the request was encoded
				// must not silently change the receipt used for a later retry.
				meta["page"].(map[string]any)["title"] = "edited after request"
				return nil, io.ErrUnexpectedEOF
			}
			if !bytes.Equal(raw, sent) {
				t.Fatalf("retry changed committed input: %s != %s", raw, sent)
			}
			body, _ = json.Marshal(media.CommitReply{Files: []media.FileInfo{{Path: "import/archive.zip", Pending: []string{"import"}}}})
		default:
			t.Fatalf("unexpected creation, transfer or publication request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})})
	_, err := client.IngestMedia(t.Context(), nil, media.RefBody{Kind: "gallery", ID: "content", Version: "edition"}, id,
		[]ck.MediaFile{{Path: "import/archive.zip", Type: "application/zip", Body: bytes.NewReader([]byte("archive")), Meta: meta}})
	var uncertain *ck.MediaCommitError
	if !errors.As(err, &uncertain) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("lost response did not retain a retryable batch/cause: %v", err)
	}
	frozen, err := json.Marshal(uncertain.Commit)
	if err != nil || !bytes.Equal(frozen, sent) {
		t.Fatalf("receipt differs from actual request: %s != %s (%v)", frozen, sent, err)
	}
	if !bytes.Contains(frozen, []byte(`"number":18014398509481987`)) || bytes.Contains(frozen, []byte("edited after request")) {
		t.Fatalf("metadata snapshot lost precision or retained caller references: %s", frozen)
	}
	if _, err := client.PostMediaUploadCommit(t.Context(), uncertain.Commit); err != nil {
		t.Fatal(err)
	}
	if presigns != 1 || commits != 2 || uncertain.Commit.OperationID != id {
		t.Fatalf("unexpected retry flow: presigns=%d commits=%d batch=%+v", presigns, commits, uncertain.Commit)
	}
}
