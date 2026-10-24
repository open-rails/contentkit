package media_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/media"
)

func TestStatusReadRequiresEditorAndCreatesNoCapabilitiesOrJobs(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.gallery(1, 3)
	f.q.take()
	status, err := f.rd.Read(t.Context(), ref, f.editor, media.ReadOptions{Status: true, Prefix: "missing/", Offset: 999, Limit: 1})
	if err != nil || status.Status == nil || status.Status.Uploads.Total != 4 || len(status.Files) != 0 || status.Total != 0 || status.Cookie != nil || status.Expires != 0 || status.Access != media.AccessNone || len(status.Public) != 0 || status.Meta != nil {
		t.Fatalf("status read leaked a capability or lost whole-manifest totals: %+v %v", status, err)
	}
	if jobs := f.q.take(); len(jobs) != 0 {
		t.Fatalf("status read queued work: %+v", jobs)
	}
	editor, err := f.rd.Read(t.Context(), ref, f.editor, media.ReadOptions{Editor: true, Prefix: "missing/", Limit: 1})
	if err != nil || !reflect.DeepEqual(editor.Status, status.Status) {
		t.Fatalf("editor read disagrees with status-only: %+v %v", editor, err)
	}
	recorder := httptest.NewRecorder()
	f.rd.Handler(media.HandlerOptions{Auth: identity{f.editor}}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/gallery/"+ref.ContentID+"?status=1", nil))
	var wire media.ReadResult
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &wire) != nil || wire.Status == nil || recorder.Header().Get("Set-Cookie") != "" || len(wire.Files) != 0 {
		t.Fatalf("HTTP status response: %d %s", recorder.Code, recorder.Body.String())
	}
	f.res.set(ref.ContentID, resolve.Resolution{Visible: true, Accessible: true})
	for _, actor := range []access.Actor{f.editor, {Anonymous: true, IP: "192.0.2.1"}} {
		if _, err := f.rd.Read(t.Context(), ref, actor, media.ReadOptions{Status: true}); !errors.Is(err, media.ErrNotVisible) {
			t.Fatalf("non-editor read status: %v", err)
		}
	}
}
