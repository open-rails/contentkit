package critical_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/notify"
	notifypg "github.com/open-rails/authkit/notify/postgres"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/media/layout"
)

// The host installs the inbox beside its real AuthKit directory and injects a
// fixed producer. The replica shares real accounts, sessions and PostgreSQL;
// no identity, notification sink or HTTP response is mocked.
type accountNoticeWorld struct {
	w            *world
	a            *application
	accounts     *authkit.Client
	accountOwner *authkit.Client
	server       *httptest.Server
	sink         notify.Publisher
	store        *notifypg.Store
	legacy       *notifypg.Store
	schema       string
	scope        string
	manager      *ck.Client
}

func newAccountNoticeWorld(t *testing.T, adoptLegacy ...bool) *accountNoticeWorld {
	t.Helper()
	w := newWorld(t, func(c *ck.Config) {
		c.Accounts = &ck.AccountsConfig{Schema: c.Database.Schema}
		c.Interactions.Limits.Disabled = true
		c.Interactions.Anonymous.Reactions = true
	})
	a := w.apps[0]
	inboxSchema := w.schema + "_inbox"
	if len(adoptLegacy) > 0 && adoptLegacy[0] {
		inboxSchema = w.schema
	} else {
		t.Cleanup(func() {
			_, err := w.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{inboxSchema}.Sanitize()+" CASCADE")
			must(t, err)
		})
	}
	scopes := map[string]string{}
	producers := []authkit.NotificationProducer{}
	for index, app := range w.apps {
		tid, err := iam.ParseTenantID(app.cfg.Scope.TenantID.String())
		must(t, err)
		scopes[tid.String()] = "contentkit:" + (layout.Scope{TenantID: tid.String()}).Partition()
		producers = append(producers, authkit.NotificationProducer{Name: fmt.Sprintf("contentkit_%d", index), TenantID: tid,
			Origin: notify.OriginScope{System: "contentkit", Scope: app.cfg.Scope.AppID.String()}, Visibility: notify.VisibilityApp, AppID: app.cfg.Scope.AppID.String()})
	}
	inbox := authtest.Replica(t, w.auth, authtest.WithConfig(func(c *authkit.Config) {
		c.Notifications = &authkit.NotificationsConfig{Schema: inboxSchema, Scopes: scopes, AppID: a.cfg.Scope.AppID.String(), Producers: producers}
	}), authtest.WithDeps(func(d *authkit.Deps) {
		d.OnPurge = func(ctx context.Context, deletion iam.UserDeletion) error {
			return a.engine.AnonymizeAccount(ctx, deletion.UserID)
		}
	}))
	tid, err := iam.ParseTenantID(a.cfg.Scope.TenantID.String())
	must(t, err)
	accounts, err := inbox.ForTenant(t.Context(), tid)
	must(t, err)
	sink, err := accounts.Notifications("contentkit_0")
	must(t, err)
	scope, err := accounts.NotificationScope()
	must(t, err)
	store, err := notifypg.New(w.pool, notifypg.Config{Schema: inboxSchema, Scope: scope})
	must(t, err)
	legacy, err := notifypg.New(w.pool, notifypg.Config{Schema: w.schema, Scope: scope})
	must(t, err)
	n := &accountNoticeWorld{w: w, a: a, accounts: accounts, accountOwner: inbox, sink: sink, store: store, legacy: legacy, schema: inboxSchema, scope: scope, server: httptest.NewServer(inbox.Handler())}
	t.Cleanup(n.server.Close)
	a.deps.Notifications = sink
	n.reopen(t)
	manager, token := w.person(t, 0)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(manager.ID), w.commentManager)
	n.manager = a.client(token)
	return n
}

func (n *accountNoticeWorld) reopen(t *testing.T) {
	t.Helper()
	n.a.server.Close()
	must(t, n.a.engine.Close(t.Context()))
	var err error
	n.a.engine, err = ck.New(t.Context(), n.a.cfg, n.a.deps)
	must(t, err)
	n.a.server = mountEngine(t, n.a.engine)
}
func (n *accountNoticeWorld) items(t *testing.T, recipient string) []notify.Item {
	t.Helper()
	page, err := n.accounts.ListNotifications(t.Context(), recipient, notify.ListOptions{Limit: 100})
	must(t, err)
	return page.Items
}
func (n *accountNoticeWorld) reply(t *testing.T, token, target, parent, body string) content.Comment {
	t.Helper()
	value, err := n.a.client(token).PostContentComments(t.Context(), target, content.CommentInput{Body: body, ReplyToID: parent}, ck.PostContentCommentsQuery{})
	must(t, err)
	return value
}
func (n *accountNoticeWorld) requireHTTPInbox(t *testing.T, token string, want int) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, n.server.URL+n.accounts.APIBase()+"/me/notifications", nil)
	must(t, err)
	authtest.Authorize(t, n.w.tenant[0], request, token)
	response, err := n.server.Client().Do(request)
	must(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	must(t, err)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("AuthKit inbox HTTP%d: %s", response.StatusCode, body)
	}
	var page notify.Page
	must(t, json.Unmarshal(body, &page))
	if len(page.Items) != want {
		t.Fatalf("AuthKit HTTP inbox count=%d want%d: %s", len(page.Items), want, body)
	}
}

func TestAccountNotificationChannel(t *testing.T) {
	n := newAccountNoticeWorld(t)
	t.Run("CK-J25-S10_injected_inbox_reply_like_and_concurrent_dedupe", func(t *testing.T) {
		owner, ownerToken := n.w.person(t, 0)
		writer, writerToken := n.w.person(t, 0)
		item := publishedArticle(t, n.a, "Inbox publication")
		parent := postComment(t, n.a.client(ownerToken), item.ID, "Parent body stays in ContentKit")
		n.reply(t, ownerToken, item.ID, parent.ID, "Own reply")
		if len(n.items(t, owner.ID)) != 0 {
			t.Fatal("self reply emitted a notice")
		}
		reply := n.reply(t, writerToken, item.ID, parent.ID, "Private comment body must not be copied")
		before := n.items(t, owner.ID)
		if len(before) != 1 || before[0].Origin.ID != "comment.reply:"+reply.ID || before[0].Visibility != notify.VisibilityApp || before[0].AppID != n.a.cfg.Scope.AppID.String() || strings.Contains(string(before[0].Data), "Private comment body") {
			t.Fatalf("wrong injected reply receipt: %+v", before)
		}
		n.requireHTTPInbox(t, ownerToken, 1)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, n.a.server.URL+"/content/v1/notifications/inbox", nil)
		must(t, err)
		authtest.Authorize(t, n.w.tenant[0], request, ownerToken)
		response, err := n.a.server.Client().Do(request)
		must(t, err)
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("ContentKit still mounts the old inbox: HTTP%d", response.StatusCode)
		}
		_, err = n.accounts.SetNotificationRead(t.Context(), owner.ID, before[0].ID, true)
		must(t, err)
		_, err = n.a.client(writerToken).PatchComment(t.Context(), reply.ID, content.CommentEdit{Body: "Edited public reply"})
		must(t, err)
		after := n.items(t, owner.ID)
		if len(after) != 1 || after[0].ID != before[0].ID || after[0].ReadAt == nil {
			t.Fatalf("active edit reset receipt identity/read state: %+v", after)
		}
		var wg sync.WaitGroup
		failures := make(chan error, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := n.a.client(writerToken).PostCommentLike(t.Context(), parent.ID)
				failures <- err
			}()
		}
		wg.Wait()
		close(failures)
		for err := range failures {
			must(t, err)
		}
		if got := n.items(t, owner.ID); len(got) != 2 {
			t.Fatalf("concurrent retries duplicated likes: %+v", got)
		}
		_, err = n.a.client(ownerToken).PostCommentLike(t.Context(), parent.ID)
		must(t, err)
		_, err = n.a.client("").PostCommentLike(t.Context(), parent.ID)
		must(t, err)
		_, err = n.a.client(writerToken).PostCommentDislike(t.Context(), parent.ID)
		must(t, err)
		if len(n.items(t, owner.ID)) != 2 {
			t.Fatal("self/anonymous/dislike emitted an extra notice")
		}
		_, err = n.a.client(writerToken).PostCommentLike(t.Context(), parent.ID)
		must(t, err)
		notices := n.items(t, owner.ID)
		sources := map[string]bool{}
		for _, notice := range notices {
			if sources[notice.Origin.ID] {
				t.Fatal("different like transitions reused identity")
			}
			sources[notice.Origin.ID] = true
		}
		if len(notices) != 3 || len(n.items(t, writer.ID)) != 0 {
			t.Fatalf("wrong transition or sender inbox: %+v", notices)
		}
		legacy, err := n.legacy.List(t.Context(), notify.Recipient{Audience: notify.AudienceUser, ID: owner.ID}, notify.ListOptions{})
		must(t, err)
		if n.schema == n.w.schema || len(legacy.Items) != 0 {
			t.Fatalf("injected publication wrote to ContentKit's fallback inbox: %+v", legacy)
		}
	})
	t.Run("CK-J25-S11_exact_delete_hold_restore_and_stale_delivery", func(t *testing.T) {
		owner, ownerToken := n.w.person(t, 0)
		_, writerToken := n.w.person(t, 0)
		item := publishedArticle(t, n.a, "Exact notification lifecycle")
		parent := postComment(t, n.a.client(ownerToken), item.ID, "Parent")
		b := n.reply(t, writerToken, item.ID, parent.ID, "Reply B")
		c := n.reply(t, writerToken, item.ID, parent.ID, "Reply C")
		before := n.items(t, owner.ID)
		if len(before) != 2 {
			t.Fatalf("missing replies: %+v", before)
		}
		must(t, n.a.client(writerToken).DeleteComment(t.Context(), b.ID))
		left := n.items(t, owner.ID)
		if len(left) != 1 || left[0].Origin.ID != "comment.reply:"+c.ID {
			t.Fatalf("deleting B withdrew wrong identity: %+v", left)
		}
		must(t, n.a.client(ownerToken).DeleteComment(t.Context(), parent.ID))
		if len(n.items(t, owner.ID)) != 0 {
			t.Fatal("deleted parent retained reply notices")
		}
		var childDeleted bool
		must(t, n.w.pool.QueryRow(t.Context(), `SELECT deleted_at IS NOT NULL FROM `+pgx.Identifier{n.w.schema, "content_comments"}.Sanitize()+` WHERE tenant_id=$1 AND id=$2`, n.a.cfg.Scope.AppID.String(), c.ID).Scan(&childDeleted))
		if childDeleted {
			t.Fatal("parent deletion recursively deleted its surviving reply")
		}
		for _, old := range before {
			must(t, n.sink.Publish(t.Context(), []notify.Publication{publicationFrom(old.Input, old.ProducerRevision)}))
		}
		if len(n.items(t, owner.ID)) != 0 {
			t.Fatal("stale delivery resurrected deleted notices")
		}
		_, err := n.a.engine.Client().PostCommentRestore(t.Context(), parent.ID)
		must(t, err)
		left = n.items(t, owner.ID)
		if len(left) != 1 || left[0].Origin.ID != "comment.reply:"+c.ID || left[0].ProducerRevision != 3 || left[0].ReadAt != nil {
			t.Fatalf("parent restore reopened wrong notice: %+v", left)
		}
		identity := left[0].ID
		must(t, n.manager.PostCommentHold(t.Context(), c.ID))
		if len(n.items(t, owner.ID)) != 0 {
			t.Fatal("held reply remained visible")
		}
		must(t, n.manager.PostCommentRelease(t.Context(), c.ID))
		left = n.items(t, owner.ID)
		if len(left) != 1 || left[0].ID != identity || left[0].ProducerRevision != 5 || left[0].ReadAt != nil {
			t.Fatalf("release did not explicitly reopen: %+v", left)
		}
		must(t, n.a.client(writerToken).DeleteComment(t.Context(), strings.ToUpper(c.ID)))
		_, err = n.a.engine.Client().PostCommentRestore(t.Context(), strings.ToUpper(c.ID))
		must(t, err)
		left = n.items(t, owner.ID)
		if len(left) != 1 || left[0].ID != identity || left[0].Origin.ID != "comment.reply:"+c.ID || left[0].ProducerRevision != 7 {
			t.Fatalf("UUID spelling changed exact identity: %+v", left)
		}
	})
	t.Run("CK-J25-S12_publication_and_withdrawal_rollback_with_source_and_erasure", func(t *testing.T) {
		owner, ownerToken := n.w.person(t, 0)
		_, writerToken := n.w.person(t, 0)
		item := publishedArticle(t, n.a, "Transactional notice")
		parent := postComment(t, n.a.client(ownerToken), item.ID, "Parent")
		table := pgx.Identifier{n.schema, "notify_items"}.Sanitize()
		// A database constraint faults the real persistence boundary. No fake sink
		// can turn a failed source transaction into a passing test.
		_, err := n.w.pool.Exec(t.Context(), `ALTER TABLE `+table+` ADD CONSTRAINT reject_new_reply CHECK (kind <> 'comment.reply') NOT VALID`)
		must(t, err)
		_, err = n.a.client(writerToken).PostContentComments(t.Context(), item.ID, content.CommentInput{Body: "Must roll back", ReplyToID: parent.ID}, ck.PostContentCommentsQuery{})
		if err == nil {
			t.Fatal("reply committed after inbox publication failed")
		}
		var replies int
		must(t, n.w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{n.w.schema, "content_comments"}.Sanitize()+` WHERE tenant_id=$1 AND reply_to_id=$2`, n.a.cfg.Scope.AppID.String(), parent.ID).Scan(&replies))
		if replies != 0 {
			t.Fatal("failed inbox publication leaked a comment")
		}
		_, err = n.w.pool.Exec(t.Context(), `ALTER TABLE `+table+` DROP CONSTRAINT reject_new_reply`)
		must(t, err)
		reply := n.reply(t, writerToken, item.ID, parent.ID, "Published")
		before := n.items(t, owner.ID)
		if len(before) != 1 {
			t.Fatalf("published notice missing: %+v", before)
		}
		_, err = n.w.pool.Exec(t.Context(), `ALTER TABLE `+table+` ADD CONSTRAINT reject_withdraw CHECK (NOT withdrawn) NOT VALID`)
		must(t, err)
		if err := n.a.client(writerToken).DeleteComment(t.Context(), reply.ID); err == nil {
			t.Fatal("deletion committed after withdrawal failed")
		}
		var deleted bool
		must(t, n.w.pool.QueryRow(t.Context(), `SELECT deleted_at IS NOT NULL FROM `+pgx.Identifier{n.w.schema, "content_comments"}.Sanitize()+` WHERE tenant_id=$1 AND id=$2`, n.a.cfg.Scope.AppID.String(), reply.ID).Scan(&deleted))
		if deleted || !reflect.DeepEqual(before, n.items(t, owner.ID)) {
			t.Fatal("failed withdrawal changed source or inbox")
		}
		_, err = n.w.pool.Exec(t.Context(), `ALTER TABLE `+table+` DROP CONSTRAINT reject_withdraw`)
		must(t, err)
		// AuthKit owns the separate inbox. Its real purge worker invokes the
		// required ContentKit cleanup before deleting the account, then keeps
		// its own durable inbox erasure fence for delayed producer delivery.
		must(t, n.accountOwner.Start(t.Context()))
		results, err := n.accounts.PurgeUsers(t.Context(), []string{owner.ID})
		must(t, err)
		for _, result := range results {
			must(t, result.Err)
		}
		waitFor(t, func() bool {
			_, err := n.accounts.User(t.Context(), iam.UserByID(owner.ID), authkit.IncludeDeleted())
			if errors.Is(err, iam.ErrUserNotFound) {
				return true
			}
			must(t, err)
			return false
		})
		must(t, n.sink.Publish(t.Context(), []notify.Publication{publicationFrom(before[0].Input, before[0].ProducerRevision)}))
		page, err := n.store.List(t.Context(), before[0].Recipient, notify.ListOptions{})
		must(t, err)
		if len(page.Items) != 0 {
			t.Fatal("delayed publication crossed account erasure fence")
		}
		if _, err := n.store.Publish(t.Context(), before[0].Input); !errors.Is(err, notify.ErrRecipientErased) {
			t.Fatalf("legacy replay crossed AuthKit's account fence: %v", err)
		}
	})
	t.Run("CK-J25-S13_reply_waits_for_concurrent_parent_deletion", func(t *testing.T) {
		owner, ownerToken := n.w.person(t, 0)
		_, writerToken := n.w.person(t, 0)
		item := publishedArticle(t, n.a, "Concurrent deletion")
		parent := postComment(t, n.a.client(ownerToken), item.ID, "Parent")
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		tx, err := n.w.pool.Begin(ctx)
		must(t, err)
		defer tx.Rollback(context.Background())
		table := pgx.Identifier{n.w.schema, "content_comments"}.Sanitize()
		_, err = tx.Exec(ctx, `UPDATE `+table+` SET deleted_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2`, n.a.cfg.Scope.AppID.String(), parent.ID)
		must(t, err)
		result := make(chan error, 1)
		go func() {
			_, err := n.a.client(writerToken).PostContentComments(ctx, item.ID, content.CommentInput{Body: "Racing reply", ReplyToID: parent.ID}, ck.PostContentCommentsQuery{})
			result <- err
		}()
		for {
			var waiting bool
			must(t, n.w.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE $1)`, "%"+table+"%").Scan(&waiting))
			if waiting {
				break
			}
			select {
			case err := <-result:
				t.Fatalf("reply completed before parent deletion: %v", err)
			case <-ctx.Done():
				t.Fatal("reply never waited for parent's lock")
			case <-time.After(10 * time.Millisecond):
			}
		}
		must(t, tx.Commit(ctx))
		if err := <-result; err == nil || !strings.Contains(err.Error(), "deleted comment") {
			t.Fatalf("reply survived committed parent deletion: %v", err)
		}
		var replies int
		must(t, n.w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table+` WHERE tenant_id=$1 AND reply_to_id=$2`, n.a.cfg.Scope.AppID.String(), parent.ID).Scan(&replies))
		if replies != 0 {
			t.Fatal("concurrent rejected reply remains stored")
		}
		if len(n.items(t, owner.ID)) != 0 {
			t.Fatal("concurrent rejected reply left a notification")
		}
	})
}

func TestAccountNotificationLegacyAdoption(t *testing.T) {
	n := newAccountNoticeWorld(t, true)
	for _, condition := range []string{"reply_deleted", "parent_deleted", "parent_held", "reply_deleted_uppercase", "parent_deleted_uppercase", "parent_deleted_duplicate"} {
		t.Run("CK-J25-S14_"+condition, func(t *testing.T) {
			state := strings.TrimSuffix(strings.TrimSuffix(condition, "_uppercase"), "_duplicate")
			owner, _ := n.w.person(t, 0)
			writer, _ := n.w.person(t, 0)
			item := publishedArticle(t, n.a, "Restored legacy thread")
			parent, reply := uuid.NewString(), uuid.NewString()
			comments := pgx.Identifier{n.w.schema, "content_comments"}.Sanitize()
			// Restore an exact pre-upgrade snapshot: source rows and legacy receipts
			// predate the notification source ledger. These direct writes are fixture
			// preparation, not substitutes for runtime or migration behavior.
			_, err := n.w.pool.Exec(t.Context(), `INSERT INTO `+comments+` (tenant_id,id,content_kind,content_id,user_id,body,deleted_at,moderation,reply_count) VALUES ($1,$2,'article',$3,$4,'Parent',CASE WHEN $5='parent_deleted' THEN now() END,CASE WHEN $5='parent_held' THEN 'held' ELSE 'approved' END,1)`, n.a.cfg.Scope.AppID.String(), parent, item.ID, owner.ID, state)
			must(t, err)
			_, err = n.w.pool.Exec(t.Context(), `INSERT INTO `+comments+` (tenant_id,id,content_kind,content_id,user_id,body,reply_to_id,deleted_at) VALUES ($1,$2,'article',$3,$4,'Reply',$5,CASE WHEN $6='reply_deleted' THEN now() END)`, n.a.cfg.Scope.AppID.String(), reply, item.ID, writer.ID, parent, state)
			must(t, err)
			legacyID := reply
			if condition != state {
				legacyID = strings.ToUpper(reply)
			}
			data, err := json.Marshal(map[string]string{"tenant_id": n.a.cfg.Scope.AppID.String(), "content_kind": "article", "content_id": item.ID, "actor_id": writer.ID, "comment_id": legacyID, "reply_to_id": parent})
			must(t, err)
			recipient := notify.Recipient{Audience: notify.AudienceUser, ID: owner.ID}
			old, err := n.store.Publish(t.Context(), notify.Input{Recipient: recipient, Origin: notify.Origin{System: "contentkit", Scope: n.a.cfg.Scope.AppID.String(), ID: "comment.reply:" + legacyID}, Kind: "comment.reply", Data: data, CreatedAt: time.Now().UTC()})
			must(t, err)
			if strings.HasSuffix(condition, "_duplicate") {
				canonical := old.Input
				canonical.Origin.ID = "comment.reply:" + reply
				canonical.CreatedAt = canonical.CreatedAt.Add(-time.Hour)
				old, err = n.store.Publish(t.Context(), canonical)
				must(t, err)
			}
			_, err = n.store.SetRead(t.Context(), recipient, old.ID, true)
			must(t, err)
			_, err = n.store.SetDismissed(t.Context(), recipient, old.ID, true)
			must(t, err)
			for range 2 {
				must(t, ck.MigratePostgres(t.Context(), n.w.pool, n.a.cfg))
			}
			var revision int64
			var withdrawn, read, archived bool
			var visibility, appID string
			must(t, n.w.pool.QueryRow(t.Context(), `SELECT producer_revision,withdrawn,read_at IS NOT NULL,dismissed_at IS NOT NULL,visibility,app_id FROM `+pgx.Identifier{n.w.schema, "notify_items"}.Sanitize()+` WHERE id=$1`, old.ID).Scan(&revision, &withdrawn, &read, &archived, &visibility, &appID))
			if revision != 2 || !withdrawn || !read || !archived || visibility != string(notify.VisibilityApp) || appID != n.a.cfg.Scope.AppID.String() {
				t.Fatalf("legacy adoption changed receipt/state/classification: rev%d hidden%v read%v archived%v visibility%s app%s", revision, withdrawn, read, archived, visibility, appID)
			}
			if got := n.items(t, owner.ID); len(got) != 0 {
				t.Fatalf("obsolete legacy receipt visible: %+v", got)
			}
			if _, err := n.accounts.GetNotification(t.Context(), owner.ID, old.ID); !errors.Is(err, notify.ErrNotFound) {
				t.Fatalf("withdrawn legacy receipt addressable: %v", err)
			}
			target := parent
			if state == "reply_deleted" {
				target = reply
			}
			if state == "parent_held" {
				must(t, n.manager.PostCommentRelease(t.Context(), target))
			} else {
				_, err = n.a.engine.Client().PostCommentRestore(t.Context(), target)
				must(t, err)
			}
			page := n.items(t, owner.ID)
			if len(page) != 1 || page[0].ID != old.ID || page[0].Origin.ID != "comment.reply:"+reply || page[0].ProducerRevision != 3 || page[0].ReadAt != nil || page[0].DismissedAt != nil {
				t.Fatalf("restoration changed original receipt identity or reopen: %+v", page)
			}
		})
	}
	t.Run("CK-J25-S15_active_legacy_classification_preserves_local_state", func(t *testing.T) {
		owner, _ := n.w.person(t, 0)
		writer, _ := n.w.person(t, 0)
		item := publishedArticle(t, n.a, "Legacy active classification")
		parent, reply := uuid.NewString(), uuid.NewString()
		_, err := n.w.pool.Exec(t.Context(), `INSERT INTO `+pgx.Identifier{n.w.schema, "content_comments"}.Sanitize()+` (tenant_id,id,content_kind,content_id,user_id,body,reply_to_id) VALUES ($1,$2,'article',$4,$5,'Parent',NULL),($1,$3,'article',$4,$6,'Reply',$2)`, n.a.cfg.Scope.AppID.String(), parent, reply, item.ID, owner.ID, writer.ID)
		must(t, err)
		data, err := json.Marshal(map[string]string{"comment_id": reply})
		must(t, err)
		recipient := notify.Recipient{Audience: notify.AudienceUser, ID: owner.ID}
		input := notify.Input{Recipient: recipient, Origin: notify.Origin{System: "contentkit", Scope: n.a.cfg.Scope.AppID.String(), ID: "comment.reply:" + reply}, Kind: "comment.reply", Data: data, CreatedAt: time.Now().UTC()}
		old, err := n.store.Publish(t.Context(), input)
		must(t, err)
		_, err = n.store.SetRead(t.Context(), recipient, old.ID, true)
		must(t, err)
		_, err = n.store.SetDismissed(t.Context(), recipient, old.ID, true)
		must(t, err)
		must(t, ck.MigratePostgres(t.Context(), n.w.pool, n.a.cfg))
		adopted, err := n.store.Get(t.Context(), recipient, old.ID)
		must(t, err)
		if adopted.Visibility != notify.VisibilityApp || adopted.AppID != n.a.cfg.Scope.AppID.String() || adopted.ReadAt == nil || adopted.DismissedAt == nil {
			t.Fatalf("classification reset local state: %+v", adopted)
		}
		must(t, n.sink.Withdraw(t.Context(), []notify.Withdrawal{{RecipientID: input.Recipient.ID, ID: input.Origin.ID, Revision: 2}}))
		must(t, n.sink.Publish(t.Context(), []notify.Publication{publicationFrom(input, 3)}))
		reopened, err := n.accounts.GetNotification(t.Context(), owner.ID, old.ID)
		must(t, err)
		if reopened.ReadAt != nil || reopened.DismissedAt != nil || reopened.ProducerRevision != 3 {
			t.Fatalf("bound publisher cannot reopen legacy receipt: %+v", reopened)
		}
	})
}
