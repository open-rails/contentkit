package contentkit_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/publishing"
)

type publishingTransport func(*http.Request) (*http.Response, error)

func (f publishingTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func publishingClient(server *httptest.Server) *ck.Client {
	return ck.NewClient(server.URL, &http.Client{Transport: publishingTransport(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer private-contentkit-key")
		return http.DefaultTransport.RoundTrip(r)
	})})
}

func publishingJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func publishingInput(id string, data []byte) ck.ArchivePublication {
	return ck.ArchivePublication{
		OperationID: id,
		File:        bytes.NewReader(data), Filename: "gallery.zip", PollInterval: time.Millisecond,
		Request: publishing.Request{
			Content:  catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: "Gallery"}}},
			Entities: []publishing.EntityReference{{Kind: "artist", Name: "Artist"}},
			Publish:  publishing.WhenReady,
		},
	}
}

func TestPublishArchiveRecoversLostResponsesWithoutLeakingKey(t *testing.T) {
	data := []byte("one complete archive")
	const id = "550e8400-e29b-41d4-a716-446655440000"
	var server *httptest.Server
	var mu sync.Mutex
	var initial []byte
	var prepares, completes, uploads, polls int
	var mutateInput func()
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/archive" && r.Header.Get("Authorization") != "Bearer private-contentkit-key" {
			t.Error("API request lost authentication")
		}
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/publishing-operations/"+id:
			prepares++
			body, _ := io.ReadAll(r.Body)
			if initial == nil {
				initial = body
				mutateInput()
				panic(http.ErrAbortHandler) // accepted, then the response was lost
			}
			if !bytes.Equal(body, initial) {
				t.Error("retry changed publishing intent")
			}
			var request publishing.Request
			if err := json.Unmarshal(body, &request); err != nil {
				t.Error(err)
			}
			sum := sha256.Sum256(data)
			if request.Archive.Size != int64(len(data)) || request.Archive.SHA256 != hex.EncodeToString(sum[:]) || request.Archive.Name != "gallery.zip" {
				t.Errorf("incorrect archive declaration: %+v", request.Archive)
			}
			publishingJSON(t, w, publishing.Operation{ID: id, State: "awaiting_upload", ContentID: "content", VersionID: "version",
				Upload: &media.PresignReply{Path: "import/source.zip", Blob: "staged", Put: &media.RequestReply{Method: http.MethodPut, URL: server.URL + "/archive"}}})
		case r.URL.Path == "/archive":
			uploads++
			if r.Header.Get("Authorization") != "" {
				t.Error("ContentKit API key leaked to object storage")
			}
			body, _ := io.ReadAll(r.Body)
			if !bytes.Equal(body, data) {
				t.Errorf("uploaded wrong bytes: %q", body)
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/complete"):
			completes++
			if completes == 1 {
				panic(http.ErrAbortHandler) // committed, but caller did not see it
			}
			publishingJSON(t, w, publishing.Operation{ID: id, ContentID: "content", VersionID: "version", State: "processing"})
		case r.Method == http.MethodGet:
			polls++
			if polls == 1 {
				publishingJSON(t, w, publishing.Operation{ID: id, ContentID: "content", VersionID: "version", State: publishing.Publishing})
				return
			}
			publishingJSON(t, w, publishing.Operation{ID: id, ContentID: "content", VersionID: "version", State: "published", Result: &publishing.OperationResult{Path: "/en/g/example", Pages: 20}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	input := publishingInput(id, data)
	mutateInput = func() { input.Request.Content.Text["en"] = catalog.Text{Title: "changed after request"} }
	var observed []string
	input.OnOperation = func(op publishing.Operation) { observed = append(observed, op.ID) }
	op, err := publishingClient(server).PublishArchive(context.Background(), input)
	if err != nil || op.State != "published" || op.Result == nil || op.Result.Pages != 20 {
		t.Fatalf("publication = %+v, %v", op, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if prepares != 2 || completes != 2 || uploads != 1 || polls != 2 || len(observed) == 0 || observed[0] != id {
		t.Fatalf("prepares=%d completes=%d uploads=%d polls=%d callbacks=%v", prepares, completes, uploads, polls, observed)
	}
}

func TestPublishArchiveResumesPartsAfterCancellation(t *testing.T) {
	data := []byte("12345678abcdefghXYZab")
	const id = "550e8400-e29b-41d4-a716-446655440001"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var server *httptest.Server
	var mu sync.Mutex
	landed := make(map[int32][]byte)
	puts := make(map[int32]int)
	interrupted := false
	aborts := 0
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/publishing-operations/"):
			publishingJSON(t, w, publishing.Operation{ID: id, State: "awaiting_upload", Upload: &media.PresignReply{Path: "import/source.zip", Blob: "staged",
				Multipart: &media.MultipartReply{Ticket: "persistent-ticket", MinPartSize: 4, MaxPartSize: 8, MaxParts: 100}}})
		case r.URL.Path == "/v1/media/upload/parts/list":
			out := media.PartsReply{}
			for n, b := range landed {
				sum := sha256.Sum256(b)
				out.Parts = append(out.Parts, media.PartReply{Number: n, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])})
			}
			publishingJSON(t, w, out)
		case r.URL.Path == "/v1/media/upload/parts":
			var request media.PartsBody
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			out := media.PartsReply{}
			for _, p := range request.Parts {
				out.Parts = append(out.Parts, media.PartReply{Number: p.Number, Request: &media.RequestReply{Method: http.MethodPut, URL: server.URL + "/part/" + strconv.Itoa(int(p.Number))}})
			}
			publishingJSON(t, w, out)
		case strings.HasPrefix(r.URL.Path, "/part/"):
			n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/part/"))
			puts[int32(n)]++
			if n == 2 && !interrupted {
				interrupted = true
				cancel()
				return
			}
			landed[int32(n)], _ = io.ReadAll(r.Body)
		case strings.HasSuffix(r.URL.Path, "/abort"):
			aborts++
		case strings.HasSuffix(r.URL.Path, "/complete"):
			if len(landed) != 3 {
				t.Errorf("completed with %d parts", len(landed))
			}
			publishingJSON(t, w, publishing.Operation{ID: id, State: "published"})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := publishingClient(server)
	op, err := client.PublishArchive(ctx, publishingInput(id, data))
	if !errors.Is(err, context.Canceled) || op.ID != id {
		t.Fatalf("cancelled publication = %+v, %v", op, err)
	}
	op, err = client.PublishArchive(context.Background(), publishingInput(id, data))
	if err != nil || op.State != "published" {
		t.Fatalf("resumed publication = %+v, %v", op, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if puts[1] != 1 || puts[2] != 2 || puts[3] != 1 || aborts != 0 {
		t.Fatalf("puts=%v, aborts=%d", puts, aborts)
	}
}

func TestPublishArchiveReturnsHeldReceiptAndFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publishingJSON(t, w, publishing.Operation{ID: "550e8400-e29b-41d4-a716-446655440002", State: "review_required",
			Error: &publishing.OperationError{Code: "moderation_required", Message: "Review required"}})
	}))
	defer server.Close()
	op, err := publishingClient(server).PublishArchive(context.Background(), publishingInput("550e8400-e29b-41d4-a716-446655440002", []byte("zip")))
	var failure *publishing.OperationError
	if !errors.As(err, &failure) || failure.Code != "moderation_required" || op.State != "review_required" {
		t.Fatalf("held publication = %+v, %v", op, err)
	}
}

func TestPublishArchiveReportsGeneratedIDBeforeFailure(t *testing.T) {
	var saved string
	input := publishingInput("", []byte("archive"))
	input.File = nil
	input.OnOperation = func(op publishing.Operation) { saved = op.ID }
	op, err := ck.NewClient("https://not-used.invalid", nil).PublishArchive(context.Background(), input)
	if err == nil || op.ID == "" || saved != op.ID {
		t.Fatalf("operation ID was lost: %+v, saved=%q, error=%v", op, saved, err)
	}
}

func TestPublishArchiveRecoversAlreadyCompletedMultipart(t *testing.T) {
	const id = "550e8400-e29b-41d4-a716-446655440005"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			publishingJSON(t, w, publishing.Operation{ID: id, State: publishing.AwaitingUpload, Upload: &media.PresignReply{
				Multipart: &media.MultipartReply{Ticket: "assembled-ticket", MinPartSize: 4, MaxPartSize: 8, MaxParts: 10},
			}})
		case strings.HasSuffix(r.URL.Path, "/parts/list"):
			w.WriteHeader(http.StatusNotFound)
			publishingJSON(t, w, map[string]string{"code": "not_found", "error": "multipart already assembled"})
		case strings.HasSuffix(r.URL.Path, "/complete"):
			publishingJSON(t, w, publishing.Operation{ID: id, State: publishing.Published})
		default:
			t.Errorf("completed multipart unexpectedly requested %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	op, err := publishingClient(server).PublishArchive(context.Background(), publishingInput(id, []byte("already uploaded")))
	if err != nil || op.State != publishing.Published || op.Upload != nil {
		t.Fatalf("completed multipart recovery = %+v, %v", op, err)
	}
}

func TestPublishArchiveDoesNotFollowStorageRedirects(t *testing.T) {
	const id = "550e8400-e29b-41d4-a716-446655440006"
	followed := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed <- struct{}{} }))
	defer target.Close()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/storage" {
			w.Header().Set("Location", target.URL)
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		publishingJSON(t, w, publishing.Operation{ID: id, State: publishing.AwaitingUpload, Upload: &media.PresignReply{
			Put: &media.RequestReply{Method: http.MethodPut, URL: server.URL + "/storage"},
		}})
	}))
	defer server.Close()
	op, err := publishingClient(server).PublishArchive(context.Background(), publishingInput(id, []byte("archive")))
	var api *ck.Error
	if !errors.As(err, &api) || api.Status != http.StatusTemporaryRedirect || op.ID != id || op.Upload != nil {
		t.Fatalf("redirect refusal = %+v, %v", op, err)
	}
	select {
	case <-followed:
		t.Fatal("followed storage redirect")
	default:
	}
}

func TestPublishArchiveRetriesStorageAndRenewsExpiredURL(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			const id = "550e8400-e29b-41d4-a716-446655440007"
			var server *httptest.Server
			var mu sync.Mutex
			prepares, uploads := 0, 0
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case strings.HasPrefix(r.URL.Path, "/storage/"):
					uploads++
					if r.Header.Get("Authorization") != "" {
						t.Error("storage received API credentials")
					}
					if uploads == 1 || status == http.StatusForbidden && r.URL.Path == "/storage/1" {
						w.WriteHeader(status)
						return
					}
				case strings.HasSuffix(r.URL.Path, "/complete"):
					publishingJSON(t, w, publishing.Operation{ID: id, State: publishing.Published})
				case r.Method == http.MethodPut:
					prepares++
					publishingJSON(t, w, publishing.Operation{ID: id, State: publishing.AwaitingUpload, Upload: &media.PresignReply{
						Put: &media.RequestReply{Method: http.MethodPut, URL: server.URL + "/storage/" + strconv.Itoa(prepares)},
					}})
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			op, err := publishingClient(server).PublishArchive(context.Background(), publishingInput(id, []byte("archive")))
			if err != nil || op.State != publishing.Published {
				t.Fatalf("storage recovery = %+v, %v", op, err)
			}
			mu.Lock()
			defer mu.Unlock()
			wantPrepares := 1
			if status == http.StatusForbidden {
				wantPrepares = 2
			}
			if prepares != wantPrepares || uploads != 2 {
				t.Fatalf("status %d: prepares=%d uploads=%d", status, prepares, uploads)
			}
		})
	}
}

func TestPublishArchiveRenewsMissingUploadWithoutNewOperation(t *testing.T) {
	const id = "550e8400-e29b-41d4-a716-446655440008"
	var mu sync.Mutex
	prepares, completes := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			prepares++
			if r.URL.Path != "/v1/publishing-operations/"+id {
				t.Errorf("changed operation while renewing: %s", r.URL.Path)
			}
			publishingJSON(t, w, publishing.Operation{ID: id, State: publishing.AwaitingUpload, Upload: &media.PresignReply{Exists: true}})
		case http.MethodPost:
			completes++
			if completes == 1 {
				w.WriteHeader(http.StatusConflict)
				publishingJSON(t, w, map[string]any{"code": "not_uploaded", "error": "upload expired", "retryable": true})
				return
			}
			publishingJSON(t, w, publishing.Operation{ID: id, State: publishing.Published})
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()
	op, err := publishingClient(server).PublishArchive(context.Background(), publishingInput(id, []byte("archive")))
	mu.Lock()
	defer mu.Unlock()
	if err != nil || op.State != publishing.Published || prepares != 2 || completes != 2 {
		t.Fatalf("missing upload recovery = %+v, %v; prepares=%d completes=%d", op, err, prepares, completes)
	}
}

func TestPublishArchiveReturnsScheduledReleaseWithoutWaitingForDate(t *testing.T) {
	const id = "550e8400-e29b-41d4-a716-446655440009"
	at := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	var mu sync.Mutex
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		if r.Method != http.MethodPut {
			t.Errorf("scheduled publication kept polling: %s", r.Method)
		}
		var request publishing.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.PublishAt == nil || !request.PublishAt.Equal(at) {
			t.Errorf("publication date was lost: %v", request.PublishAt)
		}
		publishingJSON(t, w, publishing.Operation{ID: id, State: publishing.Scheduled, PublishAt: &at})
	}))
	defer server.Close()
	input := publishingInput(id, []byte("archive"))
	input.Request.PublishAt = &at
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	op, err := publishingClient(server).PublishArchive(ctx, input)
	mu.Lock()
	defer mu.Unlock()
	if err != nil || op.State != publishing.Scheduled || op.PublishAt == nil || !op.PublishAt.Equal(at) || requests != 1 {
		t.Fatalf("scheduled publication = %+v, %v, requests=%d", op, err, requests)
	}
}
