package contentkit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/helpers/notify"
	notifypg "github.com/open-rails/helpers/notify/postgres"
)

func TestNotificationAdoptionWithdrawsAlreadyObsoleteReplies(t *testing.T) {
	for _, condition := range []string{"reply_deleted", "parent_deleted", "parent_held", "reply_deleted_uppercase", "parent_deleted_uppercase", "parent_deleted_duplicate"} {
		t.Run(condition, func(t *testing.T) {
			state := strings.TrimSuffix(strings.TrimSuffix(condition, "_uppercase"), "_duplicate")
			ctx := t.Context()
			pool := pgtest.Pool(t, nil)
			e, err := New(ctx, siteConfig(engineSchema(t, pool)), Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.Close(context.Background()) })
			parent, reply := uuid.NewString(), uuid.NewString()
			comments := pgx.Identifier{e.schema, "content_comments"}.Sanitize()
			// Simulate pre-upgrade data: old comment deletion/hiding left the
			// read receipt active and no source notification ledger existed.
			if _, err := pool.Exec(ctx, `INSERT INTO `+comments+` (tenant_id,id,content_kind,content_id,user_id,body,deleted_at,moderation,reply_count)
			 VALUES ($1,$2,'gallery',$3,'owner','parent',CASE WHEN $4='parent_deleted' THEN now() END,CASE WHEN $4='parent_held' THEN 'held' ELSE 'approved' END,1)`, e.cfg.Tenant, parent, cid(1), state); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO `+comments+` (tenant_id,id,content_kind,content_id,user_id,body,reply_to_id,deleted_at)
			 VALUES ($1,$2,'gallery',$3,'writer','reply',$4,CASE WHEN $5='reply_deleted' THEN now() END)`, e.cfg.Tenant, reply, cid(1), parent, state); err != nil {
				t.Fatal(err)
			}
			legacyReply := reply
			if condition != state {
				legacyReply = strings.ToUpper(reply)
			}
			data, _ := json.Marshal(map[string]string{"tenant_id": e.cfg.Tenant, "content_kind": "gallery", "content_id": cid(1), "actor_id": "writer", "comment_id": legacyReply, "reply_to_id": parent})
			r := notify.Recipient{Audience: notify.AudienceUser, ID: "owner"}
			old, err := e.notices.Publish(ctx, notify.Input{Recipient: r, Origin: notify.Origin{System: "contentkit", Scope: e.cfg.Tenant, ID: "comment.reply:" + legacyReply}, Kind: "comment.reply", Data: data, CreatedAt: time.Now().UTC()})
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(condition, "_duplicate") {
				canonical := old.Input
				canonical.Origin.ID = "comment.reply:" + reply
				canonical.CreatedAt = canonical.CreatedAt.Add(-time.Hour)
				old, err = e.notices.Publish(ctx, canonical)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := e.notices.SetRead(ctx, r, old.ID, true); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := e.adoptCommentNoticeIdentities(ctx); err != nil {
					t.Fatal(err)
				}
			}
			page, err := e.notices.List(ctx, r, notify.ListOptions{})
			if err != nil || len(page.Items) != 0 {
				t.Fatalf("obsolete legacy receipt remained active: %+v %v", page, err)
			}
			var revision int64
			var withdrawn, read bool
			if err := pool.QueryRow(ctx, `SELECT producer_revision,withdrawn,read_at IS NOT NULL FROM `+pgx.Identifier{e.schema, "notify_items"}.Sanitize()+` WHERE id=$1`, old.ID).Scan(&revision, &withdrawn, &read); err != nil || revision != 2 || !withdrawn || !read {
				t.Fatalf("adoption changed identity/read metadata or replayed twice: revision=%d hidden=%v read=%v err=%v", revision, withdrawn, read, err)
			}
			routes, err := e.Routes(Routes{Prefix: "/api", Groups: ContentRoutes})
			if err != nil {
				t.Fatal(err)
			}
			target, action := parent, "restore"
			status := http.StatusOK
			if state == "reply_deleted" {
				target = reply
			} else if state == "parent_held" {
				action = "release"
				status = http.StatusNoContent
			}
			serve(t, routes).call(status, access.Actor{ID: "staff", Kind: "user"}, "POST", fmt.Sprintf("/api/v1/comments/%s/%s", target, action), nil, nil)
			page, err = e.notices.List(ctx, r, notify.ListOptions{})
			if err != nil || len(page.Items) != 1 || page.Items[0].ID != old.ID || page.Items[0].Origin.ID != "comment.reply:"+reply || page.Items[0].ReadAt != nil || page.Items[0].ProducerRevision != 3 {
				t.Fatalf("restored legacy reply did not reopen its original receipt: %+v %v", page, err)
			}
		})
	}
}

func TestNotificationAdoptionClassifiesNativeAppReceipts(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	scope := Scope{TenantID: NewTenantID(), AppID: NewAppID()}
	e := &Engine{pool: pool, schema: schema, cfg: Config{Tenant: "site"}, scope: &scope}
	store, err := notifypg.New(pool, notifypg.Config{Schema: schema, Scope: "contentkit:site"})
	if err != nil {
		t.Fatal(err)
	}
	parent, reply := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "content_comments"}.Sanitize()+` (tenant_id,id,content_kind,content_id,user_id,body,reply_to_id) VALUES
	 ('site',$1,'gallery',$3,'owner','parent',NULL),('site',$2,'gallery',$3,'writer','reply',$1)`, parent, reply, cid(1)); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]string{"comment_id": reply})
	in := notify.Input{Recipient: notify.Recipient{Audience: notify.AudienceUser, ID: "owner"}, Origin: notify.Origin{System: "contentkit", Scope: "site", ID: "comment.reply:" + reply}, Kind: "comment.reply", Data: data, CreatedAt: time.Now().UTC()}
	old, err := store.Publish(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetRead(ctx, in.Recipient, old.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := e.adoptCommentNoticeIdentities(ctx); err != nil {
		t.Fatal(err)
	}
	adopted, err := store.Get(ctx, in.Recipient, old.ID)
	if err != nil || adopted.Visibility != notify.VisibilityApp || adopted.AppID != scope.AppID.String() || adopted.ReadAt == nil {
		t.Fatalf("native receipt lost classification/read state: %+v %v", adopted, err)
	}
	// This is the fixed classification AuthKit's producer handle supplies.
	in.Visibility, in.AppID = notify.VisibilityApp, scope.AppID.String()
	if err := store.Apply(ctx, []notify.Mutation{{Input: in, Revision: 2, Withdrawn: true}}); err != nil {
		t.Fatalf("bound producer could not withdraw adopted receipt: %v", err)
	}
	if err := store.Apply(ctx, []notify.Mutation{{Input: in, Revision: 3}}); err != nil {
		t.Fatalf("bound producer could not reopen adopted receipt: %v", err)
	}
	adopted, err = store.Get(ctx, in.Recipient, old.ID)
	if err != nil || adopted.ReadAt != nil || adopted.ProducerRevision != 3 {
		t.Fatalf("reopened receipt: %+v %v", adopted, err)
	}
}
