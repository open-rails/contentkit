package critical_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
)

func TestUploadAcceptanceAndRetry(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a)
	owner, credential := w.person(t, 0)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(owner.ID), w.writer)
	client := a.client(credential)
	payload := imageBytes(t)

	t.Run("CK-J09-S02_draft_progress_survives_engine_reopen", func(t *testing.T) {
		ref := galleryRef(t, client, "reopen accepted upload")
		_, err := client.IngestMedia(t.Context(), nil, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(payload)}})
		must(t, err)
		cfg := a.cfg
		cfg.Database.SkipMigrations = true
		reopened, err := ck.New(t.Context(), cfg, a.deps)
		must(t, err)
		defer func() { must(t, reopened.Close(context.Background())) }()
		ready := awaitMediaReady(t, reopened.Client(), ref)
		if ready.Status == nil || ready.Status.Uploads.Total != 1 || ready.Status.Uploads.Completed != 1 {
			t.Fatalf("reopened processing status: %+v", ready.Status)
		}
		_, err = a.client("").GetContent(t.Context(), ref.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		status, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Status: true})
		must(t, err)
		if status.Status == nil || status.Status.State != media.StateReady || len(status.Files) != 0 || status.Expires != 0 {
			t.Fatalf("status read either lost progress or issued a capability: %+v", status)
		}
	})
	t.Run("CK-J09-S03_foreign_owner_and_version_cannot_adopt_staged_bytes", func(t *testing.T) {
		ref := galleryRef(t, client, "owned upload")
		op := stageFile(t, client, ref, "pages/one.png", payload)
		_, outsider := w.person(t, 0)
		_, err := a.client(outsider).PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{op}})
		requireError(t, err, 403, "forbidden")
		other := galleryRef(t, client, "another version folder")
		_, err = client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: other, OperationID: uuid.NewString(), Ops: []media.Op{op}})
		requireError(t, err, 409, "not_uploaded")
		foreign := ref
		foreign.ID = other.ID
		_, err = client.PostMediaUploadPresign(t.Context(), media.PresignBody{Ref: foreign, Path: "pages/two.png", Type: "image/png", Size: int64(len(payload)), SHA256: digestBytes(payload)})
		requireError(t, err, 403, "forbidden")
		_, err = client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{op}})
		must(t, err)
		ready := awaitMediaReady(t, client, ref)
		requireMediaPaths(t, ready, "pages/one.png", "reader/one.webp")
		// A caller cannot choose a sibling app in either the request body or mount.
		_, err = w.apps[1].client(credential).GetContent(t.Context(), ref.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
	})
	t.Run("CK-J09-S04_invalid_type_path_size_and_checksum_leave_no_adopted_files", func(t *testing.T) {
		ref := galleryRef(t, client, "validation")
		valid := media.PresignBody{Ref: ref, Path: "pages/one.png", Type: "image/png", Size: int64(len(payload)), SHA256: digestBytes(payload)}
		for _, tc := range []struct {
			name   string
			change func(*media.PresignBody)
			status int
			code   string
		}{
			{"type", func(b *media.PresignBody) { b.Type = "application/javascript" }, 415, "type_not_allowed"},
			{"path", func(b *media.PresignBody) { b.Path = "../cover.png" }, 404, "not_found"},
			{"size", func(b *media.PresignBody) { b.Size = 2 << 20 }, 413, "too_large"},
			{"digest", func(b *media.PresignBody) { b.SHA256 = "not-a-digest" }, 400, "invalid_request"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				b := valid
				tc.change(&b)
				_, err := client.PostMediaUploadPresign(t.Context(), b)
				requireError(t, err, tc.status, tc.code)
			})
		}
		plan := uploadPlan(t, client, ref, "pages/one.png", "image/png", payload)
		corrupt := bytes.Clone(payload)
		corrupt[len(corrupt)-1] ^= 1
		code, _ := transfer(t, *plan.Put, corrupt)
		if code < 400 {
			t.Fatalf("S3 accepted checksum mismatch: %d", code)
		}
		_, err := client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}})
		requireError(t, err, 409, "not_uploaded")
		read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
		must(t, err)
		if len(read.Files) != 0 {
			t.Fatalf("rejected upload appeared in manifest: %+v", read.Files)
		}
		if code, body := transfer(t, *plan.Put, payload); code != 200 {
			t.Fatalf("valid checksum control: %d %s", code, body)
		}
	})
	t.Run("CK-J09-S05_file_count_rejection_is_atomic", func(t *testing.T) {
		ref := galleryRef(t, client, "file count boundary")
		var ops []media.Op
		for i := 0; i < 5; i++ {
			ops = append(ops, stageFile(t, client, ref, fmt.Sprintf("pages/%d.png", i), payload))
		}
		_, err := client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: ops})
		requireError(t, err, 409, "too_many_files")
		read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
		must(t, err)
		if len(read.Files) != 0 {
			t.Fatal("over-limit batch partially committed")
		}
		_, err = client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: ops[:4]})
		must(t, err)
		read = awaitMediaReady(t, client, ref)
		seen := map[string]bool{}
		for _, file := range read.Files {
			if file.Upload {
				seen[file.Path] = true
			}
		}
		for i := 0; i < 4; i++ {
			if !seen[fmt.Sprintf("pages/%d.png", i)] {
				t.Fatal("at-limit batch lost an accepted file")
			}
		}
		if seen["pages/4.png"] {
			t.Fatal("refused fifth file was adopted")
		}
	})
	t.Run("CK-J11-S01_lost_commit_response_replays_exact_batch", func(t *testing.T) {
		ref := galleryRef(t, client, "commit response lost")
		op := stageFile(t, client, ref, "pages/one.png", payload)
		batch := media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{op}}
		var accepted atomic.Bool
		proxy := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, in *http.Request) {
			request, err := http.NewRequestWithContext(in.Context(), in.Method, a.server.URL+in.URL.RequestURI(), in.Body)
			if err != nil {
				t.Error(err)
				return
			}
			request.Header = in.Header.Clone()
			res, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Error(err)
				return
			}
			defer res.Body.Close()
			_, _ = io.Copy(io.Discard, res.Body)
			if res.StatusCode != 200 {
				t.Errorf("upstream commit returned %d", res.StatusCode)
			}
			accepted.Store(res.StatusCode == 200)
			connection, _, err := out.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close()
		}))
		defer proxy.Close()
		uncertain := ck.NewClient(proxy.URL+"/content", &http.Client{Transport: bearerTransport{credential, http.DefaultTransport}})
		_, err := uncertain.PostMediaUploadCommit(t.Context(), batch)
		if err == nil || !accepted.Load() {
			t.Fatalf("did not lose an accepted response: %v accepted=%v", err, accepted.Load())
		}
		_, err = client.PostMediaUploadCommit(t.Context(), batch)
		must(t, err)
		read := awaitMediaReady(t, client, ref)
		requireMediaPaths(t, read, "pages/one.png", "reader/one.webp")
		_, err = a.client("").GetContent(t.Context(), ref.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
	})
	t.Run("CK-J11-S02_changed_batch_reusing_operation_id_conflicts", func(t *testing.T) {
		ref := galleryRef(t, client, "commit identity")
		op := stageFile(t, client, ref, "pages/one.png", payload)
		batch := media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{op}}
		_, err := client.PostMediaUploadCommit(t.Context(), batch)
		must(t, err)
		batch.Ops = []media.Op{{Op: media.OpRemove, Path: op.Path}}
		_, err = client.PostMediaUploadCommit(t.Context(), batch)
		requireError(t, err, 409, "conflict")
		read := awaitMediaReady(t, client, ref)
		requireMediaPaths(t, read, "pages/one.png", "reader/one.webp")
	})
	t.Run("CK-J09-S06_image_edit_preserves_published_access_and_source_bytes", func(t *testing.T) {
		ref := galleryRef(t, client, "immutable image edit")
		_, err := client.IngestMedia(t.Context(), nil, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(payload)}})
		must(t, err)
		_, err = client.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
		must(t, err)
		before := awaitMediaReady(t, client, ref)
		awaitEditionPublication(t, a, client, ref.ID, ref.Version)
		var source string
		for _, file := range before.Files {
			if file.Path == "pages/one.png" {
				source = file.URL
			}
		}
		if source == "" {
			t.Fatal("source URL missing")
		}
		_, err = client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpEdit, Path: "pages/one.png", Edit: &media.Edit{Rotate: 90}}}})
		must(t, err)
		awaitMediaReady(t, client, ref)
		var read media.ReadResult
		waitForWithin(t, 45*time.Second, func() bool {
			wakeDuePublicationJobs(t, a)
			time.Sleep(550 * time.Millisecond)
			read, err = a.client("").GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
			return err == nil && len(read.Files) == 1 && read.Files[0].Path == "reader/one.webp" && read.Files[0].W == 32 && read.Files[0].H == 48
		})
		assertImage(t, read.Files[0].URL, 32, 48)
		res, err := http.Get(source)
		must(t, err)
		original, err := io.ReadAll(res.Body)
		res.Body.Close()
		must(t, err)
		if res.StatusCode != 200 || !bytes.Equal(original, payload) {
			t.Fatal("editing changed or revoked the immutable source bytes")
		}
		if read.Access != media.AccessFull {
			t.Fatalf("edit changed published access: %+v", read)
		}
	})
}
