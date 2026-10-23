package contentkit

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/userinfo"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
)

func TestAccountLifecycleKeepsAnonymousContributionsAndRestoresAuthors(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	cfg := siteConfig(engineSchema(t, pool))
	provider := &replayProvider{}
	e, err := New(ctx, cfg, Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}, ProviderDataEraser: provider,
		Users: ownedFixtureRecipients{"alice": userinfo.User{ID: "alice", Username: "alice"}, "bob": userinfo.User{ID: "bob", Username: "bob"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	routes, err := e.Routes(Routes{Prefix: "/ck", Groups: e.RouteGroups()})
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t, routes)
	staff := access.Actor{ID: "staff", Kind: "user"}
	var item catalog.Content
	s.call(http.StatusCreated, staff, "POST", "/ck/v1/contents", catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: "Retained contributions"}}, Versions: []catalog.VersionInput{{Languages: []string{"en"}}}}, &item)
	s.call(http.StatusOK, staff, "POST", "/ck/v1/contents/"+item.ID+"/publish", catalog.PublishInput{}, nil)
	path := "/ck/v1/contents/" + item.ID
	for _, id := range []string{"alice", "bob"} {
		actor := access.Actor{ID: id, Kind: "user"}
		s.call(http.StatusOK, actor, "POST", path+"/like", nil, nil)
		s.call(http.StatusOK, actor, "POST", path+"/favorite", nil, nil)
		s.call(http.StatusCreated, actor, "POST", path+"/comments", content.CommentInput{Body: "discussion by " + id}, nil)
	}
	assertTotals := func() {
		t.Helper()
		var likes, favorites, comments int
		err := pool.QueryRow(ctx, `SELECT likes,favorites,comment_count FROM `+pgx.Identifier{e.schema, "content_interaction_counts"}.Sanitize()+` WHERE tenant_id=$1 AND content_kind='gallery' AND content_id=$2 AND content_version_id IS NULL`, e.cfg.Tenant, item.ID).Scan(&likes, &favorites, &comments)
		if err != nil || likes != 2 || favorites != 2 || comments != 2 {
			t.Fatalf("account lifecycle changed counts: likes=%d favorites=%d comments=%d err=%v", likes, favorites, comments, err)
		}
	}
	readComments := func() []content.Comment {
		t.Helper()
		var comments []content.Comment
		s.call(http.StatusOK, staff, "GET", path+"/comments", nil, &comments)
		return comments
	}
	if err := e.SetAccountDeleted(ctx, "alice", true); err != nil {
		t.Fatal(err)
	}
	for _, c := range readComments() {
		if c.UserID == "alice" && (c.Author == nil || !c.Author.Deleted || c.Author.Username != "") {
			t.Fatalf("soft-deleted author still shown: %+v", c)
		}
	}
	assertTotals()
	if err := e.SetAccountDeleted(ctx, "alice", false); err != nil {
		t.Fatal(err)
	}
	for _, c := range readComments() {
		if c.UserID == "alice" && (c.Author == nil || c.Author.Deleted || c.Author.Username != "alice") {
			t.Fatalf("restored author missing: %+v", c)
		}
	}
	provider.fail = true
	if err := e.AnonymizeAccount(ctx, "alice"); err == nil {
		t.Fatal("provider failure was acknowledged")
	}
	var pendingKey string
	if err := pool.QueryRow(ctx, `SELECT anonymous_key::text FROM `+pgx.Identifier{e.schema, "content_account_lifecycle"}.Sanitize()+` WHERE tenant_id=$1 AND actor_id='alice' AND state='purging'`, e.cfg.Tenant).Scan(&pendingKey); err != nil || pendingKey == "" {
		t.Fatalf("interrupted anonymization lost retry key: %q %v", pendingKey, err)
	}
	assertTotals()
	provider.fail = false
	for _, id := range []string{"alice", "alice", "bob", "bob"} {
		if err := e.AnonymizeAccount(ctx, id); err != nil {
			t.Fatal(err)
		}
		assertTotals()
	}
	for _, c := range readComments() {
		if c.UserID != "" || c.Author != nil || c.AnonName != "<Deleted>" || c.Deleted || c.Body == "" {
			t.Fatalf("hard deletion lost discussion or kept attribution: %+v", c)
		}
	}
	for _, table := range []string{"content_reactions", "content_favorites"} {
		var anonymous int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{e.schema, table}.Sanitize()+` WHERE tenant_id=$1 AND user_id IS NULL AND anonymized`, e.cfg.Tenant).Scan(&anonymous); err != nil || anonymous != 2 {
			t.Fatalf("anonymous %s contributions=%d err=%v", table, anonymous, err)
		}
	}
	var mappings int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{e.schema, "content_account_lifecycle"}.Sanitize()+` WHERE tenant_id=$1 AND anonymous_key IS NOT NULL`, e.cfg.Tenant).Scan(&mappings); err != nil || mappings != 0 {
		t.Fatalf("finished anonymization retains identity mapping: %d %v", mappings, err)
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return e.content.ImportReactions(ctx, tx, []content.ImportReaction{{Ref: contentref.New(e.cfg.Tenant, "gallery", item.ID), UserID: "alice", Value: 1}})
	})
	if !errors.Is(err, content.ErrSubjectErased) {
		t.Fatalf("removed account could write again: %v", err)
	}
}
