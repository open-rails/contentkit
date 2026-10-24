package media_test

import (
	"bufio"
	"context"
	"github.com/open-rails/contentkit/internal/resolve"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/helpers/eventlog"
)

type progressCredential struct{}

func (progressCredential) Identity() auth.Identity {
	return auth.Identity{Subject: "editor", SubjectKind: auth.SubjectUser}
}
func (progressCredential) Can(context.Context, auth.Scope, string) (bool, error) { return false, nil }

type progressAuthenticator struct{}

func (progressAuthenticator) Authenticate(*http.Request) (auth.Verified, error) {
	return progressCredential{}, nil
}
func (progressAuthenticator) RootScope(context.Context) (auth.Scope, error) {
	return auth.Scope{Authority: "test", ID: "root"}, nil
}

func TestProgressHandlerAuthorizedSnapshotHasNoCapabilities(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	f.gallery(1, 1)
	var logReads atomic.Int32
	server := httptest.NewServer(f.rd.Handler(media.HandlerOptions{Auth: progressAuthenticator{}, Events: func(contentref.ContentRef) (eventlog.Reader, error) {
		return eventlog.ReaderFunc(func(context.Context, eventlog.Query) (eventlog.Page, error) {
			logReads.Add(1)
			return eventlog.Page{NextCursor: "cursor"}, nil
		}), nil
	}}))
	defer server.Close()
	request := func(id string) *http.Response {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		t.Cleanup(cancel)
		req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/gallery/"+id+"?status=1", nil)
		req.Header.Set("Accept", "text/event-stream")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	hidden := request(cid(2))
	hidden.Body.Close()
	if hidden.StatusCode != 404 || logReads.Load() != 0 {
		t.Fatalf("hidden item observed: status=%d logReads=%d", hidden.StatusCode, logReads.Load())
	}
	response := request(cid(1))
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("status=%d", response.StatusCode)
	}
	if response.Header.Get("Set-Cookie") != "" {
		t.Fatal("status stream minted cookie")
	}
	scanner := bufio.NewScanner(response.Body)
	var snapshot strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		snapshot.WriteString(line + "\n")
		if line == "" {
			break
		}
	}
	value := snapshot.String()
	if !strings.Contains(value, "event: snapshot") || !strings.Contains(value, `"status":`) || !strings.Contains(value, `"files":[]`) {
		t.Fatalf("snapshot=%s", value)
	}
	if strings.Contains(value, `"url":`) || strings.Contains(value, "?t=") {
		t.Fatalf("snapshot contains media capability: %s", value)
	}
	f.res.set(cid(1), resolve.Resolution{Visible: true, Accessible: true, Editor: false})
	var refusal strings.Builder
	for scanner.Scan() {
		refusal.WriteString(scanner.Text() + "\n")
	}
	if !strings.Contains(refusal.String(), "event: error") || !strings.Contains(refusal.String(), "not_found") {
		t.Fatalf("editor permission change did not close stream: %s", refusal.String())
	}

}
