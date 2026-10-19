package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
)

// Publishing waits for media: with no worker rendering here, a book whose
// published version has a page still producing refuses to publish or
// schedule (not_ready), and so does making such a version live on a published
// book; a version without uploads publishes as before.
func TestPublishWaitsForMedia(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	f := serve(t, w.engine(nil))
	book := func(state string) catalog.Content {
		t.Helper()
		var c catalog.Content
		f.want(http.StatusCreated, staff, "POST", "/contents", catalog.ContentInput{Kind: "book", Text: map[string]catalog.Text{"en": {Title: "Waiting " + state}},
			Versions: []catalog.VersionInput{{Languages: []string{"en"}, State: state}}}, &c)
		return c
	}
	upload := func(c catalog.Content, path string) {
		t.Helper()
		body := pngBytes(t, 8)
		ref := media.RefBody{Kind: c.Kind, ID: c.ID, Version: c.Versions[0].ID}
		sum := sha256.Sum256(body)
		var plan media.PresignReply
		f.want(http.StatusOK, staff, "POST", "/media/upload/presign", media.PresignBody{Ref: ref, Path: path, Type: "image/png", Size: int64(len(body)),
			SHA256: hex.EncodeToString(sum[:])}, &plan)
		req, _ := http.NewRequest(plan.Put.Method, plan.Put.URL, bytes.NewReader(body))
		for k, v := range plan.Put.Headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		f.want(http.StatusOK, staff, "POST", "/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
			Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}}, nil)
	}
	refused := func(method, target string, body any) {
		t.Helper()
		rec := f.want(http.StatusConflict, staff, method, target, body, nil)
		var refusal struct{ Code, Error string }
		if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil || refusal.Code != "not_ready" || !strings.HasPrefix(refusal.Error, "media not ready: pages are still") {
			t.Fatalf("%s %s: %s", method, target, rec.Body)
		}
	}

	waiting := book("")
	upload(waiting, "pages/001")
	refused("POST", "/contents/"+waiting.ID+"/publish", catalog.PublishInput{})
	later := time.Now().Add(time.Hour)
	refused("POST", "/contents/"+waiting.ID+"/publish", catalog.PublishInput{PublishAt: &later})

	empty := book("draft")
	f.want(http.StatusOK, staff, "POST", "/contents/"+empty.ID+"/publish", catalog.PublishInput{}, nil)
	upload(empty, "pages/001")
	refused("PATCH", "/contents/"+empty.ID+"/versions/"+empty.Versions[0].ID, catalog.VersionInput{State: "published"})
}
