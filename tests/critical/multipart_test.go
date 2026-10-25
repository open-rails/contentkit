package critical_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/media"
)

func TestMultipartUpload(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a)
	owner, credential := w.person(t, 0)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(owner.ID), w.writer)
	client := a.client(credential)
	// The public protocol chooses multipart only above 64 MiB. Exercise that
	// real boundary; do not replace storage or change the production threshold.
	payload := bytes.Repeat([]byte("multipart source\n"), (media.MaxSinglePut/(len("multipart source\n")))+1)
	t.Run("CK-J10-S01_partial_transfer_reopens_and_resumes", func(t *testing.T) {
		ref := galleryRef(t, client, "multipart resume")
		plan := uploadPlan(t, client, ref, "attachments/resume.txt", "text/plain", payload)
		if plan.Multipart == nil || plan.Put != nil {
			t.Fatal("large upload did not use multipart")
		}
		ticket := media.TicketBody{Ticket: plan.Multipart.Ticket}
		part := func(t *testing.T, client *ck.Client, n int32, start, end int) {
			t.Helper()
			chunk := payload[start:end]
			parts, err := client.PostMediaUploadParts(t.Context(), media.PartsBody{Ticket: ticket.Ticket, Parts: []media.PartBody{{Number: n, Size: int64(len(chunk)), SHA256: digestBytes(chunk)}}})
			must(t, err)
			if len(parts.Parts) != 1 || parts.Parts[0].Request == nil {
				t.Fatal("missing presigned part")
			}
			if code, body := transfer(t, *parts.Parts[0].Request, chunk); code != 200 {
				t.Fatalf("part %d: %d %s", n, code, body)
			}
		}
		part(t, client, 1, 0, media.MinPartSize)
		// Reopen the actual service with existing stores, then continue through
		// its public host client. The actor-bound ticket still needs the same
		// person, so mount the reopened engine behind real HTTP for the retry.
		cfg := a.cfg
		cfg.Database.SkipMigrations = true
		reopened, err := ck.New(t.Context(), cfg, a.deps)
		must(t, err)
		defer func() { must(t, reopened.Close(context.Background())) }()
		server := mountEngine(t, reopened)
		resumed := ck.NewClient(server.URL+"/content", &http.Client{Transport: bearerTransport{credential, http.DefaultTransport}})
		landed, err := resumed.PostMediaUploadPartsList(t.Context(), ticket)
		must(t, err)
		if len(landed.Parts) != 1 || landed.Parts[0].Number != 1 || landed.Parts[0].Size != media.MinPartSize || landed.Parts[0].SHA256 != digestBytes(payload[:media.MinPartSize]) {
			t.Fatalf("resumed parts lost durable integrity: %+v", landed)
		}
		_, err = resumed.PostMediaUploadComplete(t.Context(), ticket)
		requireError(t, err, 409, "incomplete")
		for start, n := media.MinPartSize, int32(2); start < len(payload); start, n = start+media.MinPartSize, n+1 {
			part(t, resumed, n, start, min(start+media.MinPartSize, len(payload)))
		}
		completed, err := resumed.PostMediaUploadComplete(t.Context(), ticket)
		must(t, err)
		if completed.Blob != plan.Blob || completed.Size != int64(len(payload)) || completed.Type != "text/plain" {
			t.Fatalf("wrong completed object: %+v", completed)
		}
		t.Run("CK-J10-S05_complete_replay_preserves_object_identity", func(t *testing.T) {
			again, err := resumed.PostMediaUploadComplete(t.Context(), ticket)
			must(t, err)
			if again != completed {
				t.Fatalf("completion replay changed allocation: %+v != %+v", again, completed)
			}
		})
		_, err = resumed.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: completed.Blob}}})
		must(t, err)
		read := awaitMediaReady(t, resumed, ref)
		if len(read.Files) != 1 || read.Files[0].Path != plan.Path {
			t.Fatalf("unexpected adopted multipart file: %+v", read.Files)
		}
		res, err := http.Get(read.Files[0].URL)
		must(t, err)
		got, err := io.ReadAll(res.Body)
		res.Body.Close()
		must(t, err)
		if res.StatusCode != 200 || !bytes.Equal(got, payload) {
			t.Fatal("resumed multipart changed source bytes")
		}
	})
	t.Run("CK-J10-S02_part_checksum_mismatch_cannot_complete", func(t *testing.T) {
		ref := galleryRef(t, client, "multipart checksum")
		plan := uploadPlan(t, client, ref, "attachments/checksum.txt", "text/plain", payload)
		ticket := media.TicketBody{Ticket: plan.Multipart.Ticket}
		chunk := bytes.Clone(payload[:media.MinPartSize])
		parts, err := client.PostMediaUploadParts(t.Context(), media.PartsBody{Ticket: ticket.Ticket, Parts: []media.PartBody{{Number: 1, Size: int64(len(chunk)), SHA256: digestBytes(chunk)}}})
		must(t, err)
		chunk[0] ^= 1
		if code, _ := transfer(t, *parts.Parts[0].Request, chunk); code < 400 {
			t.Fatalf("S3 accepted corrupt part: %d", code)
		}
		_, err = client.PostMediaUploadComplete(t.Context(), ticket)
		requireError(t, err, 409, "incomplete")
		must(t, client.PostMediaUploadAbort(t.Context(), ticket))
	})
	t.Run("CK-J10-S03_incomplete_object_is_not_adoptable", func(t *testing.T) {
		ref := galleryRef(t, client, "multipart incomplete")
		plan := uploadPlan(t, client, ref, "attachments/incomplete.txt", "text/plain", payload)
		_, err := client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}})
		requireError(t, err, 409, "not_uploaded")
		read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
		must(t, err)
		if len(read.Files) != 0 {
			t.Fatal("incomplete multipart appeared in manifest")
		}
		must(t, client.PostMediaUploadAbort(t.Context(), media.TicketBody{Ticket: plan.Multipart.Ticket}))
	})
	t.Run("CK-J10-S04_foreign_actor_cannot_reuse_ticket", func(t *testing.T) {
		ref := galleryRef(t, client, "multipart owner")
		plan := uploadPlan(t, client, ref, "attachments/owner.txt", "text/plain", payload)
		other, credential := w.person(t, 0)
		authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(other.ID), w.writer)
		foreign := a.client(credential)
		ticket := media.TicketBody{Ticket: plan.Multipart.Ticket}
		_, err := foreign.PostMediaUploadPartsList(t.Context(), ticket)
		requireError(t, err, 403, "forbidden")
		_, err = foreign.PostMediaUploadComplete(t.Context(), ticket)
		requireError(t, err, 403, "forbidden")
		err = foreign.PostMediaUploadAbort(t.Context(), ticket)
		requireError(t, err, 403, "forbidden")
		_, err = client.PostMediaUploadPartsList(t.Context(), ticket)
		must(t, err)
		must(t, client.PostMediaUploadAbort(t.Context(), ticket))
	})
	t.Run("CK-J10-S06_abort_refuses_late_part_and_commit", func(t *testing.T) {
		ref := galleryRef(t, client, "multipart abort")
		plan := uploadPlan(t, client, ref, "attachments/aborted.txt", "text/plain", payload)
		ticket := media.TicketBody{Ticket: plan.Multipart.Ticket}
		chunk := payload[:media.MinPartSize]
		parts, err := client.PostMediaUploadParts(t.Context(), media.PartsBody{Ticket: ticket.Ticket, Parts: []media.PartBody{{Number: 1, Size: int64(len(chunk)), SHA256: digestBytes(chunk)}}})
		must(t, err)
		must(t, client.PostMediaUploadAbort(t.Context(), ticket))
		if code, _ := transfer(t, *parts.Parts[0].Request, chunk); code != 404 {
			t.Fatalf("late part after abort returned %d", code)
		}
		_, err = client.PostMediaUploadComplete(t.Context(), ticket)
		requireError(t, err, 404, "not_found")
		_, err = client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}})
		requireError(t, err, 409, "not_uploaded")
		read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
		must(t, err)
		if len(read.Files) != 0 {
			t.Fatal("aborted multipart was still adopted into the version")
		}
	})
}
