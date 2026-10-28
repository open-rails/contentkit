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

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/notify"
	notifypg "github.com/open-rails/authkit/notify/postgres"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/importer"
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
	schema       string
	scope        string
	manager      *ck.Client
}

func newAccountNoticeWorld(t *testing.T) *accountNoticeWorld {
	t.Helper()
	w := newWorld(t, func(c *ck.Config) {
		c.Accounts = &ck.AccountsConfig{Schema: c.Database.Schema}
		c.Interactions.Limits.Disabled = true
		c.Interactions.Anonymous.Reactions = true
	})
	a := w.apps[0]
	inboxSchema := w.schema + "_inbox"
	t.Cleanup(func() {
		_, err := w.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{inboxSchema}.Sanitize()+" CASCADE")
		must(t, err)
	})
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
	n := &accountNoticeWorld{w: w, a: a, accounts: accounts, accountOwner: inbox, sink: sink, store: store, schema: inboxSchema, scope: scope, server: httptest.NewServer(inbox.Handler())}
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
		_, err := n.accounts.SetNotificationRead(t.Context(), owner.ID, before[0].ID, true)
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
		if len(n.items(t, owner.ID)) != 1 {
			t.Fatal("dislike did not withdraw the like notice")
		}
		_, err = n.a.client(writerToken).PostCommentLike(t.Context(), parent.ID)
		must(t, err)
		notices := n.items(t, owner.ID)
		sources := map[string]bool{}
		for _, notice := range notices {
			if sources[notice.Origin.ID] {
				t.Fatal("duplicate like notice identity")
			}
			sources[notice.Origin.ID] = true
		}
		if len(notices) != 2 || len(n.items(t, writer.ID)) != 0 {
			t.Fatalf("wrong transition or sender inbox: %+v", notices)
		}
		likeID := "comment.liked:" + parent.ID + ":" + writer.ID
		if !sources[likeID] {
			t.Fatalf("missing stable like identity: %+v", notices)
		}
		_, err = n.a.client(writerToken).DeleteCommentReaction(t.Context(), parent.ID)
		must(t, err)
		if len(n.items(t, owner.ID)) != 1 {
			t.Fatal("unlike did not withdraw the like notice")
		}
		_, err = n.a.client(writerToken).PostCommentLike(t.Context(), parent.ID)
		must(t, err)
		if got := n.items(t, owner.ID); len(got) != 2 {
			t.Fatalf("re-like duplicated the notice: %+v", got)
		}
		var identities int
		must(t, n.w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{n.w.schema, "content_comment_notifications"}.Sanitize()+` WHERE tenant_id=$1 AND source_id=$2`, n.a.cfg.Scope.AppID.String(), likeID).Scan(&identities))
		if identities != 1 {
			t.Fatalf("like identities=%d; want 1", identities)
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
			t.Fatalf("direct replay crossed AuthKit's account fence: %v", err)
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

func TestHiddenCommentAuthors(t *testing.T) {
	n := newAccountNoticeWorld(t)
	a := n.a
	owner, ownerToken := n.w.person(t, 0)
	writer, writerToken := n.w.person(t, 0)
	ownerClient, writerClient, public := a.client(ownerToken), a.client(writerToken), a.client("")
	item := publishedArticle(t, a, "Hidden authors")
	parent := postComment(t, ownerClient, item.ID, "Visible parent")
	top := postComment(t, writerClient, item.ID, "Hidden top level")
	reply := n.reply(t, writerToken, item.ID, parent.ID, "Hidden reply")
	child := n.reply(t, ownerToken, item.ID, top.ID, "Visible child")
	held := postComment(t, writerClient, item.ID, "Held before hiding")
	approvedLater := postComment(t, writerClient, item.ID, "Approve while hidden")
	must(t, n.manager.PostCommentHold(t.Context(), held.ID))
	must(t, n.manager.PostCommentHold(t.Context(), approvedLater.ID))
	_, err := ownerClient.PostCommentLike(t.Context(), top.ID)
	must(t, err)
	_, err = ownerClient.PostCommentLike(t.Context(), reply.ID)
	must(t, err)
	if len(n.items(t, owner.ID)) != 1 || len(n.items(t, writer.ID)) != 3 {
		t.Fatal("missing initial reply/like notices")
	}
	checkCounts := func(wantTop, wantReplies int) {
		t.Helper()
		var count int
		must(t, n.w.pool.QueryRow(t.Context(), `SELECT comment_count FROM `+pgx.Identifier{n.w.schema, "content_interaction_counts"}.Sanitize()+` WHERE tenant_id=$1 AND content_kind='article' AND content_id=$2 AND content_version_id IS NULL`, a.cfg.Scope.AppID.String(), item.ID).Scan(&count))
		if count != wantTop {
			t.Fatalf("comment_count=%d; want %d", count, wantTop)
		}
		if got := ownComment(t, ownerClient, item.ID, parent.ID).ReplyCount; got != wantReplies {
			t.Fatalf("reply_count=%d; want %d", got, wantReplies)
		}
		standing, err := public.GetContentCanComment(t.Context(), item.ID, ck.GetContentCanCommentQuery{})
		must(t, err)
		if standing.Count == nil || *standing.Count != wantTop {
			t.Fatalf("standing count=%v; want %d", standing.Count, wantTop)
		}
	}
	checkCounts(2, 1)
	for range 2 {
		must(t, a.engine.SetAccountHidden(t.Context(), writer.ID, true))
		checkCounts(1, 0)
	}
	assertCommentVisibility(t, public, item.ID, top.ID, false)
	assertCommentVisibility(t, writerClient, item.ID, top.ID, true)
	replies, err := public.GetCommentReplies(t.Context(), parent.ID, ck.GetCommentRepliesQuery{})
	must(t, err)
	if len(replies) != 0 {
		t.Fatalf("hidden reply visible: %+v", replies)
	}
	latest, err := public.GetCommentsLatest(t.Context(), ck.GetCommentsLatestQuery{Limit: 100})
	must(t, err)
	for _, cm := range latest {
		if cm.UserID == writer.ID {
			t.Fatalf("hidden author in latest: %+v", cm)
		}
	}
	admin, err := n.manager.GetCommentsAdmin(t.Context(), ck.GetCommentsAdminQuery{Author: writer.ID})
	must(t, err)
	if len(admin) != 4 {
		t.Fatalf("admin lost hidden comments: %+v", admin)
	}
	for _, cm := range admin {
		if !cm.AuthorHidden {
			t.Fatalf("admin omitted hidden state: %+v", cm)
		}
	}
	queue, err := n.manager.GetModerationHeld(t.Context(), ck.GetModerationHeldQuery{Kind: content.KindComment})
	must(t, err)
	if len(queue.Items) != 2 {
		t.Fatalf("hidden held comments missing: %+v", queue)
	}
	if len(n.items(t, owner.ID)) != 0 || len(n.items(t, writer.ID)) != 0 {
		t.Fatal("hiding retained reply/like notices")
	}
	must(t, n.manager.PostCommentRelease(t.Context(), approvedLater.ID))
	created := postComment(t, writerClient, item.ID, "Created while hidden")
	assertCommentVisibility(t, public, item.ID, created.ID, false)
	must(t, n.manager.PostCommentHold(t.Context(), created.ID))
	checkCounts(1, 0)
	must(t, n.manager.PostCommentRelease(t.Context(), created.ID))
	_, err = writerClient.PatchComment(t.Context(), created.ID, content.CommentEdit{Body: "Edited while hidden"})
	must(t, err)
	must(t, writerClient.DeleteComment(t.Context(), top.ID))
	_, err = n.manager.PostCommentRestore(t.Context(), top.ID)
	must(t, err)
	checkCounts(1, 0)
	if len(n.items(t, writer.ID)) != 0 {
		t.Fatal("restoring while hidden published notices")
	}
	for range 2 {
		must(t, a.engine.SetAccountHidden(t.Context(), writer.ID, false))
		checkCounts(4, 1)
	}
	for _, cm := range []content.Comment{top, approvedLater, created} {
		assertCommentVisibility(t, public, item.ID, cm.ID, true)
	}
	assertCommentVisibility(t, public, item.ID, held.ID, false)
	if ownComment(t, writerClient, item.ID, held.ID).Moderation != content.ModerationHeld {
		t.Fatal("unhide changed the pre-existing hold")
	}
	replies, err = public.GetCommentReplies(t.Context(), parent.ID, ck.GetCommentRepliesQuery{})
	must(t, err)
	if len(replies) != 1 || replies[0].ID != reply.ID {
		t.Fatalf("unhide did not restore replies: %+v", replies)
	}
	latest, err = public.GetCommentsLatest(t.Context(), ck.GetCommentsLatestQuery{Limit: 100})
	must(t, err)
	seen := map[string]bool{}
	for _, cm := range latest {
		seen[cm.ID] = true
	}
	for _, cm := range []content.Comment{top, reply, child, approvedLater, created} {
		if !seen[cm.ID] {
			t.Fatalf("unhide did not restore latest comment %s", cm.ID)
		}
	}
	if len(n.items(t, owner.ID)) != 1 || len(n.items(t, writer.ID)) != 3 {
		t.Fatal("unhide did not restore reply/like notices")
	}
	must(t, a.engine.SetAccountDeleted(t.Context(), writer.ID, true))
	must(t, a.engine.SetAccountHidden(t.Context(), writer.ID, true))
	must(t, a.engine.SetAccountHidden(t.Context(), writer.ID, false))
	if got := ownComment(t, public, item.ID, top.ID); got.AnonName != "<Deleted>" || got.UserID != "" {
		t.Fatalf("unhide changed deletion presentation: %+v", got)
	}
}

func TestHiddenCommentAuthorReactions(t *testing.T) {
	n := newAccountNoticeWorld(t)
	a := n.a
	owner, ownerToken := n.w.person(t, 0)
	writer, writerToken := n.w.person(t, 0)
	ownerClient, writerClient, public := a.client(ownerToken), a.client(writerToken), a.client("")
	item := publishedArticle(t, a, "Hidden reactions")
	second := publishedArticle(t, a, "Reactions made while hidden")
	postComment(t, writerClient, item.ID, "Author and reactor on the same content")
	parent := postComment(t, ownerClient, item.ID, "Existing like")
	fresh := postComment(t, ownerClient, item.ID, "New hidden like")
	negative := postComment(t, ownerClient, item.ID, "New hidden dislike")
	imported := postComment(t, ownerClient, item.ID, "Imported hidden like")
	_, err := writerClient.PostCommentLike(t.Context(), parent.ID)
	must(t, err)
	_, err = writerClient.PostContentLike(t.Context(), item.ID, ck.PostContentLikeQuery{})
	must(t, err)
	_, err = public.PostCommentLike(t.Context(), parent.ID)
	must(t, err)
	_, err = public.PostContentLike(t.Context(), item.ID, ck.PostContentLikeQuery{})
	must(t, err)
	_, err = writerClient.PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
	must(t, err)

	checkCounts := func(kind, id string, likes, dislikes int) {
		t.Helper()
		var gotLikes, gotDislikes int
		must(t, n.w.pool.QueryRow(t.Context(), `SELECT coalesce(sum(likes),0),coalesce(sum(dislikes),0) FROM `+pgx.Identifier{n.w.schema, "content_interaction_counts"}.Sanitize()+` WHERE tenant_id=$1 AND content_kind=$2 AND content_id=$3 AND content_version_id IS NULL`, a.cfg.Scope.AppID.String(), kind, id).Scan(&gotLikes, &gotDislikes))
		if gotLikes != likes || gotDislikes != dislikes {
			t.Fatalf("%s/%s counts=%d/%d; want %d/%d", kind, id, gotLikes, gotDislikes, likes, dislikes)
		}
		if kind == content.KindComment {
			cm := ownComment(t, ownerClient, item.ID, id)
			if cm.Likes != likes || cm.Dislikes != dislikes {
				t.Fatalf("comment columns=%d/%d; want %d/%d", cm.Likes, cm.Dislikes, likes, dislikes)
			}
		}
	}
	checkNotices := func(ids ...string) {
		t.Helper()
		got := n.items(t, owner.ID)
		want := map[string]bool{}
		for _, id := range ids {
			want["comment.liked:"+id+":"+writer.ID] = true
		}
		if len(got) != len(want) {
			t.Fatalf("notices=%+v; want %v", got, want)
		}
		for _, notice := range got {
			if !want[notice.Origin.ID] {
				t.Fatalf("unexpected notice: %+v", notice)
			}
		}
	}
	importReactions := func(rows ...importer.ReactionRecord) {
		t.Helper()
		out, err := a.engine.Client().PostImport(t.Context(), importer.Batch{Source: "hidden-reactions", Reactions: rows})
		must(t, err)
		if len(out.Failed) != 0 || out.Written[importer.SectionReactions] != len(rows) {
			t.Fatalf("reaction import failed: %+v", out)
		}
	}
	recount := func() {
		t.Helper()
		var rows []importer.ReactionRecord
		for _, id := range []string{parent.ID, fresh.ID, negative.ID, imported.ID} {
			rows = append(rows, importer.ReactionRecord{Target: importer.TargetRef{Comment: &importer.Ref{ID: id}}, IP: "203.0.113.44", Value: 0})
		}
		for _, id := range []string{item.ID, second.ID} {
			rows = append(rows, importer.ReactionRecord{Target: importer.TargetRef{Content: &importer.Ref{ID: id}}, IP: "203.0.113.44", Value: 0})
		}
		importReactions(rows...)
	}
	checkHidden := func() {
		t.Helper()
		checkCounts(content.KindComment, parent.ID, 1, 0)
		for _, id := range []string{fresh.ID, negative.ID, imported.ID} {
			checkCounts(content.KindComment, id, 0, 0)
		}
		checkCounts("article", item.ID, 1, 0)
		checkCounts("article", second.ID, 0, 0)
		checkNotices()
		standing, err := public.GetContentCanComment(t.Context(), item.ID, ck.GetContentCanCommentQuery{})
		must(t, err)
		if standing.Count == nil || *standing.Count != 4 {
			t.Fatalf("hidden author's comment rollup=%v; want 4", standing.Count)
		}
		fav, err := writerClient.GetContentFavorite(t.Context(), item.ID, ck.GetContentFavoriteQuery{})
		must(t, err)
		if !fav.Favorited || fav.Count == nil || *fav.Count != 1 {
			t.Fatalf("hiding changed favorite: %+v", fav)
		}
	}
	checkCounts(content.KindComment, parent.ID, 2, 0)
	checkCounts("article", item.ID, 2, 0)
	checkNotices(parent.ID)
	for range 2 {
		must(t, a.engine.SetAccountHidden(t.Context(), writer.ID, true))
		checkHidden()
	}
	_, err = writerClient.PostCommentLike(t.Context(), fresh.ID)
	must(t, err)
	_, err = writerClient.DeleteCommentReaction(t.Context(), fresh.ID)
	must(t, err)
	checkHidden()
	_, err = writerClient.PostCommentLike(t.Context(), fresh.ID)
	must(t, err)
	_, err = writerClient.PostCommentDislike(t.Context(), negative.ID)
	must(t, err)
	_, err = writerClient.PostContentDislike(t.Context(), item.ID, ck.PostContentDislikeQuery{})
	must(t, err)
	_, err = writerClient.PostContentLike(t.Context(), second.ID, ck.PostContentLikeQuery{})
	must(t, err)
	importReactions(importer.ReactionRecord{Target: importer.TargetRef{Comment: &importer.Ref{ID: imported.ID}}, UserID: writer.ID, Value: 1})
	checkHidden()
	recount()
	checkHidden()
	for id, value := range map[string]int16{parent.ID: 1, fresh.ID: 1, negative.ID: -1, imported.ID: 1} {
		if got := ownComment(t, writerClient, item.ID, id).Mine; got != value {
			t.Fatalf("hidden reaction %s mine=%d; want %d", id, got, value)
		}
	}
	state, err := writerClient.GetContentReaction(t.Context(), item.ID, ck.GetContentReactionQuery{})
	must(t, err)
	if state.Mine != -1 {
		t.Fatalf("hidden content reaction was not stored: %+v", state)
	}
	_, err = public.PostCommentDislike(t.Context(), parent.ID)
	must(t, err)
	checkCounts(content.KindComment, parent.ID, 0, 1)
	_, err = public.PostCommentLike(t.Context(), parent.ID)
	must(t, err)
	checkHidden()

	for range 2 {
		for range 2 {
			must(t, a.engine.SetAccountHidden(t.Context(), writer.ID, false))
			checkCounts(content.KindComment, parent.ID, 2, 0)
			checkCounts(content.KindComment, fresh.ID, 1, 0)
			checkCounts(content.KindComment, negative.ID, 0, 1)
			checkCounts(content.KindComment, imported.ID, 1, 0)
			checkCounts("article", item.ID, 1, 1)
			checkCounts("article", second.ID, 1, 0)
			checkNotices(parent.ID, fresh.ID)
			standing, err := public.GetContentCanComment(t.Context(), item.ID, ck.GetContentCanCommentQuery{})
			must(t, err)
			if standing.Count == nil || *standing.Count != 5 {
				t.Fatalf("restored author's comment rollup=%v; want 5", standing.Count)
			}
		}
		for range 2 {
			must(t, a.engine.SetAccountHidden(t.Context(), writer.ID, true))
			checkHidden()
		}
	}
	must(t, a.engine.AnonymizeAccount(t.Context(), writer.ID))
	must(t, a.engine.SetAccountHidden(t.Context(), writer.ID, false))
	recount()
	checkCounts(content.KindComment, parent.ID, 1, 0)
	for _, id := range []string{fresh.ID, negative.ID, imported.ID} {
		checkCounts(content.KindComment, id, 0, 0)
	}
	checkCounts("article", item.ID, 1, 0)
	checkCounts("article", second.ID, 0, 0)
	checkNotices()
	var retained int
	must(t, n.w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{n.w.schema, "content_reactions"}.Sanitize()+` WHERE tenant_id=$1 AND anonymous_actor_id IS NOT NULL AND user_id IS NULL AND author_hidden AND value<>0`, a.cfg.Scope.AppID.String()).Scan(&retained))
	if retained != 6 {
		t.Fatalf("hidden anonymized reactions=%d; want 6", retained)
	}
}

func TestHiddenReactorNoticeVisibility(t *testing.T) {
	n := newAccountNoticeWorld(t)
	a := n.a
	owner, ownerToken := n.w.person(t, 0)
	writer, writerToken := n.w.person(t, 0)
	item := publishedArticle(t, a, "Hidden reactor notice eligibility")
	held := postComment(t, a.client(ownerToken), item.ID, "Held target")
	deleted := postComment(t, a.client(ownerToken), item.ID, "Deleted target")
	hidden := postComment(t, a.client(ownerToken), item.ID, "Hidden target")
	must(t, a.engine.SetAccountHidden(t.Context(), writer.ID, true))
	for _, id := range []string{held.ID, deleted.ID, hidden.ID} {
		_, err := a.client(writerToken).PostCommentLike(t.Context(), id)
		must(t, err)
	}
	must(t, n.manager.PostCommentHold(t.Context(), held.ID))
	must(t, n.manager.PostCommentRelease(t.Context(), held.ID))
	if got := n.items(t, owner.ID); len(got) != 0 {
		t.Fatalf("release notified for a hidden reactor: %+v", got)
	}
	must(t, n.manager.PostCommentHold(t.Context(), held.ID))
	must(t, a.client(ownerToken).DeleteComment(t.Context(), deleted.ID))
	must(t, a.engine.SetAccountHidden(t.Context(), owner.ID, true))
	must(t, a.engine.SetAccountHidden(t.Context(), writer.ID, false))
	if got := n.items(t, owner.ID); len(got) != 0 {
		t.Fatalf("unhiding reactor notified for invisible comments: %+v", got)
	}
	must(t, a.engine.SetAccountHidden(t.Context(), owner.ID, false))
	if got := n.items(t, owner.ID); len(got) != 1 || got[0].Origin.ID != "comment.liked:"+hidden.ID+":"+writer.ID {
		t.Fatalf("unhiding author restored ineligible notices: %+v", got)
	}
	must(t, n.manager.PostCommentRelease(t.Context(), held.ID))
	_, err := n.manager.PostCommentRestore(t.Context(), deleted.ID)
	must(t, err)
	if got := n.items(t, owner.ID); len(got) != 3 {
		t.Fatalf("restoring comments lost likes made while hidden: %+v", got)
	}
}
