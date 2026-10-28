package critical_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/gateway"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
)

func TestMediaAdmissionBoundaries(t *testing.T) {
	t.Run("CK-J10-S04_expired_issuer_ticket_cannot_read_complete_abort_or_presign_parts", func(t *testing.T) {
		m := newResilientMedia(t)
		client := m.app.engine.Client()
		ref := galleryRef(t, client, "Expiring multipart capability")
		plan, err := client.PostMediaUploadPresign(t.Context(), media.PresignBody{Ref: ref, Path: "attachments/expiry.txt", Type: "text/plain", Size: media.MaxSinglePut + 1, SHA256: strings.Repeat("ab", 32)})
		must(t, err)
		if plan.Multipart == nil {
			t.Fatal("large source was not allocated as multipart")
		}
		original := media.TicketBody{Ticket: plan.Multipart.Ticket}
		_, err = client.PostMediaUploadPartsList(t.Context(), original)
		must(t, err)
		// This is a legitimate issuer-owned expired credential for the real
		// stored upload, not a malformed signature or an absent object. The
		// normal engine lifetime is24h, so never sleep that long in CI.
		payload, _, ok := strings.Cut(original.Ticket, ".")
		if !ok {
			t.Fatal("issuer ticket has no signed payload")
		}
		data, err := base64.RawURLEncoding.DecodeString(payload)
		must(t, err)
		var claims map[string]json.RawMessage
		must(t, json.Unmarshal(data, &claims))
		expires := time.Now().Truncate(time.Second)
		claims["e"], err = json.Marshal(expires.Unix())
		must(t, err)
		data, err = json.Marshal(claims)
		must(t, err)
		payload = base64.RawURLEncoding.EncodeToString(data)
		ring, err := token.NewRing(m.storage.key, nil)
		must(t, err)
		expired := media.TicketBody{Ticket: payload + "." + ring.Sign("upload|"+payload, expires)}
		_, err = client.PostMediaUploadPartsList(t.Context(), expired)
		requireError(t, err, 400, "invalid_request")
		_, err = client.PostMediaUploadParts(t.Context(), media.PartsBody{Ticket: expired.Ticket, Parts: []media.PartBody{{Number: 1, Size: media.MinPartSize, SHA256: strings.Repeat("cd", 32)}}})
		requireError(t, err, 400, "invalid_request")
		_, err = client.PostMediaUploadComplete(t.Context(), expired)
		requireError(t, err, 400, "invalid_request")
		err = client.PostMediaUploadAbort(t.Context(), expired)
		requireError(t, err, 400, "invalid_request")
		listed, err := client.PostMediaUploadPartsList(t.Context(), original)
		must(t, err)
		if len(listed.Parts) != 0 {
			t.Fatal("expired credential changed the multipart upload")
		}
		read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
		must(t, err)
		if len(read.Files) != 0 {
			t.Fatal("expired ticket published bytes")
		}
		must(t, client.PostMediaUploadAbort(t.Context(), original))
		_, err = client.PostMediaUploadPartsList(t.Context(), original)
		requireError(t, err, 404, "not_found")
	})
	t.Run("CK-J09-S05_entity_owner_quota_is_atomic_and_removal_refunds_once", func(t *testing.T) {
		m := newResilientMedia(t, func(c *ck.Config) {
			c.Content.Channels.Uploads.QuotaBytes = 64 << 10
		})
		_, credential, entity := ownedChannel(t, m.world, m.app, "Quota-owned media")
		client := m.app.client(credential)
		ref := media.RefBody{Kind: "channel", ID: string(entity.ID)}
		first := stageFile(t, client, ref, "cover/first.png", imageBytes(t))
		second := stageFile(t, client, ref, "cover/second.png", imageBytes(t))
		_, err := client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{first}})
		must(t, err)
		batch := media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{second}}
		_, err = client.PostMediaUploadCommit(t.Context(), batch)
		requireError(t, err, 413, "quota_exceeded")
		read, err := client.GetMedia(t.Context(), ref.Kind, ref.ID, ck.GetMediaQuery{Editor: true})
		must(t, err)
		requireMediaPaths(t, read, first.Path)
		remove := media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpRemove, Path: first.Path}}}
		for range 2 {
			_, err = client.PostMediaUploadCommit(t.Context(), remove)
			must(t, err)
		}
		_, err = client.PostMediaUploadCommit(t.Context(), batch)
		must(t, err)
		_, err = client.PostMediaUploadPresign(t.Context(), media.PresignBody{Ref: ref, Path: "cover/third.png", Type: "image/png", Size: int64(len(imageBytes(t))), SHA256: digestBytes(imageBytes(t))})
		requireError(t, err, 413, "quota_exceeded")
		read, err = client.GetMedia(t.Context(), ref.Kind, ref.ID, ck.GetMediaQuery{Editor: true})
		must(t, err)
		requireMediaPaths(t, read, second.Path)
		_, otherCredential, otherEntity := ownedChannel(t, m.world, m.app, "Independent media quota")
		other := m.app.client(otherCredential)
		otherRef := media.RefBody{Kind: "channel", ID: string(otherEntity.ID)}
		op := stageFile(t, other, otherRef, "cover/other.png", imageBytes(t))
		_, err = other.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: otherRef, OperationID: uuid.NewString(), Ops: []media.Op{op}})
		must(t, err)
		otherRead, err := other.GetMedia(t.Context(), otherRef.Kind, otherRef.ID, ck.GetMediaQuery{Editor: true})
		must(t, err)
		requireMediaPaths(t, otherRead, op.Path)
	})
	t.Run("CK-J09-S03_two_apps_share_storage_without_sharing_upload_allocations", func(t *testing.T) {
		m := newResilientMedia(t)
		b := m.apps[1]
		b.server.Close()
		must(t, b.engine.Close(t.Context()))
		workerSchema, hostSchema := m.schema+"_other_worker", m.schema+"_other_river"
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			for _, schema := range []string{workerSchema, hostSchema} {
				_, err := m.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
				if err != nil {
					t.Errorf("drop second app queue: %v", err)
				}
			}
		})
		gw := m.storage.gatewayConfig
		gw.Routes = []gateway.Route{{Host: "127.0.0.1", Scope: layout.Scope{TenantID: b.cfg.Scope.TenantID.String(), AppID: b.cfg.Scope.AppID.String()}, Bucket: m.storage.bucket}}
		handler, err := gateway.New(gw)
		must(t, err)
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		config := *m.app.cfg.Media
		config.BaseURL = server.URL
		config.WorkerSchema = workerSchema
		b.cfg.Media = &config
		b.cfg.Database.RiverSchema = hostSchema
		b.cfg.Database.SkipMigrations = false
		b.deps.Store = m.app.deps.Store // the same physical bucket and S3 connection
		must(t, ck.MigratePostgres(t.Context(), m.pool, b.cfg))
		b.cfg.Database.SkipMigrations = true
		b.engine, err = ck.New(t.Context(), b.cfg, b.deps)
		must(t, err)
		b.server = mountEngine(t, b.engine)
		must(t, b.engine.StartJobs(t.Context()))
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			b.server.Close()
			if err := b.engine.Close(ctx); err != nil {
				t.Errorf("close second app: %v", err)
			}
		})
		user, credential := m.person(t, 0)
		for _, app := range []*application{m.app, b} {
			authtest.GrantRole(t, m.tenant[0], app.group, iam.UserSubject(user.ID), m.writer)
		}
		first, second := m.app.client(credential), b.client(credential)
		firstRef, secondRef := galleryRef(t, first, "App A media"), galleryRef(t, second, "App B media")
		aUpload := stageFile(t, first, firstRef, "pages/a.png", imageBytes(t))
		bUpload := stageFile(t, second, secondRef, "pages/b.png", imageBytes(t))
		_, err = first.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: firstRef, OperationID: uuid.NewString(), Ops: []media.Op{aUpload}})
		must(t, err)
		_, err = second.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: secondRef, OperationID: uuid.NewString(), Ops: []media.Op{bUpload}})
		must(t, err)
		_, err = second.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: firstRef, OperationID: uuid.NewString(), Ops: []media.Op{aUpload}})
		requireError(t, err, 403, "forbidden")
		stolen := aUpload
		stolen.Path = "pages/stolen.png"
		_, err = second.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: secondRef, OperationID: uuid.NewString(), Ops: []media.Op{stolen}})
		requireError(t, err, 409, "not_uploaded")
		_, err = second.GetMediaVersion(t.Context(), firstRef.Kind, firstRef.ID, firstRef.Version, ck.GetMediaVersionQuery{Editor: true})
		requireError(t, err, 404, "not_found")
		for _, control := range []struct {
			client *ck.Client
			ref    media.RefBody
			path   string
		}{{first, firstRef, aUpload.Path}, {second, secondRef, bUpload.Path}} {
			read, err := control.client.GetMediaVersion(t.Context(), control.ref.Kind, control.ref.ID, control.ref.Version, ck.GetMediaVersionQuery{Editor: true})
			must(t, err)
			requireMediaPaths(t, read, control.path)
		}
	})
}
