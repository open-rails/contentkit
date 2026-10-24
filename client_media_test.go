package contentkit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
