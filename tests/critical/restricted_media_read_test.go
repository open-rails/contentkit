package critical_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

func TestRestrictedMediaReadIsNotEditAuthority(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a, func(c *ck.Config) {
		kind := c.Content.Kinds["gallery"]
		kind.Download = &kinds.Download{Off: true}
		c.Content.Kinds["gallery"] = kind
		c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	admin := a.engine.Client()
	ref := galleryRef(t, admin, "Restricted draft media")
	payload := imageBytes(t)
	_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(payload)}})
	must(t, err)
	awaitMediaReady(t, admin, ref)
	later := w.clock.now().Add(time.Hour)
	_, err = admin.PutContentVersionSegments(t.Context(), ref.ID, ref.Version, catalog.SegmentsInput{Segments: []catalog.SegmentInput{{Start: 0, End: 1, ReleaseAt: &later}}})
	must(t, err)
	auditor, token := w.person(t, 0)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(auditor.ID), w.viewer)
	reader := a.client(token)
	draft, err := reader.GetContent(t.Context(), ref.ID, ck.GetContentQuery{})
	must(t, err)
	if draft.Editor || !draft.Decision.Allowed {
		t.Fatalf("auditor draft standing=%+v editor=%v", draft.Decision, draft.Editor)
	}
	read, err := reader.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
	must(t, err)
	var rendition string
	for _, file := range read.Files {
		if file.EditorURL != "" {
			t.Fatal("auditor received an editor-only source URL")
		}
		if file.Path == "reader/one.webp" {
			rendition = file.URL
			assertImage(t, file.URL, file.W, file.H)
		}
	}
	if rendition == "" || len(read.Uploads) != 0 {
		t.Fatalf("restricted reader lost private image or gained editing controls: %+v", read)
	}
	planResponse, err := reader.GetContentVersionReader(t.Context(), ref.ID, ref.Version)
	must(t, err)
	var plan versions.ReaderPlan
	must(t, json.Unmarshal(planResponse.Body, &plan))
	pages := 0
	for _, segment := range plan.Segments {
		if segment.Locked {
			t.Fatal("read-restricted auditor could not inspect an unreleased chapter")
		}
		pages += len(segment.Pages)
	}
	if pages != 1 {
		t.Fatalf("auditor reader plan has %d pages, want1", pages)
	}
	_, err = reader.PostMediaUploadPresign(t.Context(), media.PresignBody{Ref: ref, Path: "pages/refused.png", Type: "image/png", Size: int64(len(payload)), SHA256: digestBytes(payload)})
	requireError(t, err, 403, "forbidden")
	err = reader.PutContentVersionReader(t.Context(), ref.ID, ref.Version, versions.ReaderMeta{})
	requireError(t, err, 403, "forbidden")
	_, err = reader.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpRemove, Path: "pages/one.png"}}})
	requireError(t, err, 403, "forbidden")
	_, err = a.client("").GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
	requireError(t, err, 404, "not_found")
	authtest.RevokeRole(t, w.tenant[0], a.group, iam.UserSubject(auditor.ID), w.viewer)
	_, err = reader.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
	requireError(t, err, 404, "not_found")
}
