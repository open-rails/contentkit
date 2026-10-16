package contentkit

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/signal"
)

// Interactions target ContentKit's own content items, their versions and
// groups through /v1/contents/{id}/… and /v1/entities/{id}/…: comments in
// language threads, reactions and favorites; the reader's favorites and
// reactions come back as cards; counts reach the listing; recent searches
// are kept, cleared, pruned and erased.
func TestEngineInteractions(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := engineSchema(t, pool)
	cfg := siteConfig(schema)
	cfg.Content.Languages = []string{"en", "ja"}
	e, err := New(ctx, cfg, Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	routes, err := e.Routes(Routes{Prefix: "/ck", Groups: e.RouteGroups()})
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t, routes)
	const v = "/ck/v1"
	staff, u1, u2 := access.Actor{ID: "staff", Kind: "user"}, access.Actor{ID: "u1", Kind: "user"}, access.Actor{ID: "u2", Kind: "user"}
	anon := access.Actor{Anonymous: true, IP: "10.0.0.2"}

	create := func(title string, publish bool) catalog.Content {
		t.Helper()
		var c catalog.Content
		s.call(http.StatusCreated, staff, "POST", v+"/contents", catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: title}},
			Versions: []catalog.VersionInput{{Languages: []string{"en"}}}}, &c)
		if publish {
			s.call(http.StatusOK, staff, "POST", v+"/contents/"+c.ID+"/publish", catalog.PublishInput{}, nil)
		}
		s.call(http.StatusOK, staff, "GET", v+"/contents/"+c.ID, nil, &c)
		return c
	}
	harbor, draft, third := create("Harbor", true), create("Draft", false), create("Third", true)
	var tags []groups.Group
	tag := func(slug, name string) groups.GroupInput {
		return groups.GroupInput{Kind: "tag", Slug: slug, Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: name}}}
	}
	s.call(http.StatusCreated, staff, "POST", v+"/entities", []groups.GroupInput{tag("moonlit", "Moonlit"), tag("rainy", "Rainy"), tag("old", "Old")}, &tags)
	moon, rainy, old := string(tags[0].ID), string(tags[1].ID), string(tags[2].ID)

	// Comments join a language's thread; a reply joins its comment's.
	item := v + "/contents/" + harbor.ID
	var en, reply content.Comment
	s.call(http.StatusCreated, u1, "POST", item+"/comments", content.CommentInput{Body: "hello", Language: "en"}, &en)
	s.call(http.StatusCreated, u2, "POST", item+"/comments", content.CommentInput{Body: "konnichiwa", Language: "ja"}, nil)
	s.call(http.StatusCreated, u2, "POST", item+"/comments", content.CommentInput{Body: "hi back", ReplyToID: en.ID}, &reply)
	if reply.Language != "en" {
		t.Fatalf("reply language %q", reply.Language)
	}
	var list []content.Comment
	s.call(http.StatusOK, anon, "GET", item+"/comments?language=ja", nil, &list)
	if len(list) != 1 || list[0].Body != "konnichiwa" || list[0].Language != "ja" {
		t.Fatalf("ja thread %+v", list)
	}
	s.call(http.StatusBadRequest, u1, "POST", item+"/comments", content.CommentInput{Body: "x", Language: "fr"}, nil)
	s.call(http.StatusNotFound, u1, "POST", v+"/contents/"+draft.ID+"/comments", content.CommentInput{Body: "x"}, nil)
	s.call(http.StatusNotFound, u1, "POST", v+"/contents/"+cid(9)+"/like", nil, nil)
	s.call(http.StatusNotFound, u1, "POST", v+"/contents/"+moon+"/like", nil, nil)

	// The built-in moderator masks staff's banned words.
	s.call(http.StatusForbidden, u1, "POST", v+"/moderation/banned-words", []content.BannedWordInput{{Word: "darn"}}, nil)
	s.call(http.StatusOK, staff, "POST", v+"/moderation/banned-words", []content.BannedWordInput{{Word: "darn"}}, nil)
	var masked content.Comment
	s.call(http.StatusCreated, u2, "POST", item+"/comments", content.CommentInput{Body: "darn it"}, &masked)
	if masked.Body != "**** it" {
		t.Fatalf("masked %q", masked.Body)
	}

	// A version is its own target.
	vid := harbor.Versions[0].ID
	var counts content.ReactionCounts
	s.call(http.StatusOK, u1, "POST", item+"/like?version="+vid, nil, &counts)
	if *counts.Likes != 1 || counts.Mine != 1 {
		t.Fatalf("version like %+v", counts)
	}
	s.call(http.StatusOK, u1, "GET", item+"/reaction", nil, &counts)
	if *counts.Likes != 0 {
		t.Fatalf("the item counts its version's like: %+v", counts)
	}
	s.call(http.StatusNotFound, u1, "POST", item+"/like?version="+cid(5), nil, nil)
	s.call(http.StatusOK, u1, "POST", item+"/like", nil, nil)
	s.call(http.StatusOK, u2, "POST", item+"/dislike", nil, nil)
	var fav content.FavoriteState
	s.call(http.StatusOK, u1, "POST", item+"/favorite", nil, nil)
	s.call(http.StatusOK, u2, "POST", item+"/favorite", nil, &fav)
	if !fav.Favorited || fav.Count == nil || *fav.Count != 2 {
		t.Fatalf("favorite %+v", fav)
	}
	s.call(http.StatusOK, u1, "POST", v+"/contents/"+third.ID+"/favorite", nil, nil)

	// Groups take reactions, favorites and comments; a merged group answers
	// as its survivor, a deleted one as nothing.
	s.call(http.StatusOK, u1, "POST", v+"/entities/"+moon+"/like", nil, nil)
	s.call(http.StatusOK, u1, "POST", v+"/entities/"+rainy+"/dislike", nil, nil)
	s.call(http.StatusOK, u1, "POST", v+"/entities/"+moon+"/favorite", nil, nil)
	s.call(http.StatusCreated, u1, "POST", v+"/entities/"+moon+"/comments", content.CommentInput{Body: "love this tag"}, nil)
	var standing content.CommentStanding
	s.call(http.StatusOK, u1, "GET", v+"/entities/"+moon+"/can-comment", nil, &standing)
	if !standing.CanComment {
		t.Fatalf("standing %+v", standing)
	}
	s.call(http.StatusBadRequest, u1, "POST", v+"/entities/"+moon+"/like?version="+vid, nil, nil)
	s.call(http.StatusOK, staff, "POST", v+"/entities/"+old+"/merge", groups.MergeInput{Into: groups.GroupID(moon)}, nil)
	s.call(http.StatusOK, u2, "POST", v+"/entities/"+old+"/like", nil, &counts)
	if *counts.Likes != 2 {
		t.Fatalf("a like through the merged group %+v", counts)
	}
	s.call(http.StatusOK, staff, "DELETE", v+"/entities/"+rainy, nil, nil)
	s.call(http.StatusNotFound, u2, "POST", v+"/entities/"+rainy+"/like", nil, nil)

	// The reader's reactions and favorites, as cards.
	var page discovery.InteractionPage
	mine := func(a access.Actor, path string) {
		t.Helper()
		page = discovery.InteractionPage{}
		s.call(http.StatusOK, a, "GET", v+path, nil, &page)
	}
	mine(u1, "/me/reactions?target=entities&kind=tag")
	if len(page.Items) != 1 || page.Items[0].Group == nil || page.Items[0].Group.Name != "Moonlit" || page.Items[0].Value != 1 {
		t.Fatalf("tag reactions %+v", page)
	}
	mine(u2, "/me/reactions?target=entities&value=like")
	if len(page.Items) != 1 || string(page.Items[0].Group.ID) != moon {
		t.Fatalf("u2's tag likes %+v", page)
	}
	mine(u1, "/me/reactions?value=like")
	if len(page.Items) != 1 || page.Items[0].Content == nil || page.Items[0].Content.ID != harbor.ID {
		t.Fatalf("u1's content likes %+v", page)
	}
	mine(u2, "/me/reactions?value=dislike")
	if len(page.Items) != 1 || page.Items[0].Value != -1 || page.Items[0].Content.Title != "Harbor" {
		t.Fatalf("u2's dislikes %+v", page)
	}
	s.call(http.StatusBadRequest, u1, "GET", v+"/me/reactions?value=meh", nil, nil)
	s.call(http.StatusUnauthorized, anon, "GET", v+"/me/favorites", nil, nil)
	mine(u1, "/me/favorites?limit=1")
	if len(page.Items) != 1 || page.Items[0].Content.ID != third.ID || page.NextCursor == "" {
		t.Fatalf("favorites page 1 %+v", page)
	}
	mine(u1, "/me/favorites?limit=1&cursor="+url.QueryEscape(page.NextCursor))
	if len(page.Items) != 1 || page.Items[0].Content.ID != harbor.ID || page.NextCursor != "" {
		t.Fatalf("favorites page 2 %+v", page)
	}
	mine(u1, "/me/favorites?target=entities")
	if len(page.Items) != 1 || string(page.Items[0].Group.ID) != moon {
		t.Fatalf("group favorites %+v", page)
	}
	s.call(http.StatusOK, staff, "POST", v+"/contents/"+third.ID+"/unpublish", nil, nil)
	mine(u1, "/me/favorites")
	if len(page.Items) != 1 || page.Items[0].Content.ID != harbor.ID {
		t.Fatalf("an unpublished favorite still lists: %+v", page)
	}

	// Counts reach the listing.
	if _, err := e.catalog.RefreshCounts(ctx); err != nil {
		t.Fatal(err)
	}
	var listed discovery.ContentPage
	s.call(http.StatusOK, anon, "GET", v+"/contents?kind=gallery&sort=most_favorited", nil, &listed)
	if len(listed.Contents) == 0 || listed.Contents[0].ID != harbor.ID || *listed.Contents[0].Counts.Likes != 1 ||
		*listed.Contents[0].Counts.Dislikes != 1 || *listed.Contents[0].Counts.Favorites != 2 {
		t.Fatalf("listing %+v", listed.Contents)
	}
	var rating float64
	if err := pool.QueryRow(ctx, `SELECT rating FROM `+pgx.Identifier{schema, "content_facets"}.Sanitize()+` WHERE content_id = $1`, harbor.ID).Scan(&rating); err != nil || rating <= 0 {
		t.Fatalf("rating %v %v", rating, err)
	}

	// Recent searches: kept per reader, one per query, newest first.
	search := func(a access.Actor, q string) {
		t.Helper()
		s.call(http.StatusOK, a, "GET", v+"/search?"+q, nil, nil)
	}
	recent := func(a access.Actor, q string) []string {
		t.Helper()
		var rs discovery.RecentSearches
		s.call(http.StatusOK, a, "GET", v+"/me/searches"+q, nil, &rs)
		var out []string
		for _, r := range rs.Searches {
			out = append(out, r.Query)
		}
		return out
	}
	search(u1, "q=Harbor")
	search(u1, "q=moonlit")
	search(u1, "q=harbor%20%20")
	search(anon, "q=secret&session=tab-1")
	search(anon, "q=nobody")
	if got := recent(u1, ""); len(got) != 2 || got[0] != "harbor" || got[1] != "moonlit" {
		t.Fatalf("u1 recent %v", got)
	}
	if got := recent(anon, "?session=tab-1"); len(got) != 1 || got[0] != "secret" {
		t.Fatalf("session recent %v", got)
	}
	s.call(http.StatusBadRequest, anon, "GET", v+"/me/searches", nil, nil)
	s.call(http.StatusNoContent, u1, "DELETE", v+"/me/searches?query=moonlit", nil, nil)
	if got := recent(u1, ""); len(got) != 1 || got[0] != "harbor" {
		t.Fatalf("after removing one %v", got)
	}
	s.call(http.StatusNoContent, u1, "DELETE", v+"/me/searches", nil, nil)
	if got := recent(u1, ""); len(got) != 0 {
		t.Fatalf("after clearing %v", got)
	}
	search(u1, "q=again")
	if got := recent(u1, ""); len(got) != 1 || got[0] != "again" {
		t.Fatalf("after a new search %v", got)
	}
	searches := pgx.Identifier{schema, "content_searches"}.Sanitize()
	rows := func(subject string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+searches+` WHERE subject = $1`, subject).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n, err := e.discover.PruneSearches(ctx); err != nil || n != 1 || rows("u1") != 1 {
		t.Fatalf("prune %d %v, u1 keeps %d", n, err, rows("u1"))
	}

	// Erasure takes recent searches with the rest.
	var session string
	if err := pool.QueryRow(ctx, `SELECT subject FROM `+searches+` WHERE subject LIKE 's\_%'`).Scan(&session); err != nil {
		t.Fatal(err)
	}
	report, err := e.EraseSubjects(ctx, []signal.Subject{{UserID: "u1"}, {AnonKey: session}})
	if err != nil || !report.Complete() {
		t.Fatalf("erasure %+v %v", report, err)
	}
	search(u1, "q=after")
	if rows("u1") != 0 || rows(session) != 0 {
		t.Fatalf("searches after erasure: u1 %d, session %d", rows("u1"), rows(session))
	}
	mine(u1, "/me/reactions")
	if len(page.Items) != 0 {
		t.Fatalf("an erased reader's reactions %+v", page)
	}
}

// A content item's comments setting closes or hides its comments, and the
// request's site scope applies to direct reads and interactions: content a
// site excludes is not found there.
func TestEngineCommentsSettingAndSiteScope(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := engineSchema(t, pool)
	cfg := siteConfig(schema)
	cfg.Content.Sites = []kinds.Site{{Name: "main", Hosts: []string{"example.com"}}, {Name: "safe", Hosts: []string{"safe.example"}, ExcludeRestrictions: []string{"restricted"}}}
	e, err := New(ctx, cfg, Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	routes, err := e.Routes(Routes{Prefix: "/ck", Groups: e.RouteGroups()})
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t, routes)
	const v = "/ck/v1"
	staff, u1 := access.Actor{ID: "staff", Kind: "user"}, access.Actor{ID: "u1", Kind: "user"}
	create := func(in catalog.ContentInput) catalog.Content {
		t.Helper()
		var c catalog.Content
		in.Kind, in.Text, in.Versions = "gallery", map[string]catalog.Text{"en": {Title: "T"}}, []catalog.VersionInput{{Languages: []string{"en"}}}
		s.call(http.StatusCreated, staff, "POST", v+"/contents", in, &c)
		s.call(http.StatusOK, staff, "POST", v+"/contents/"+c.ID+"/publish", catalog.PublishInput{}, &c)
		return c
	}

	c := create(catalog.ContentInput{})
	if c.Comments != content.CommentsOpen {
		t.Fatalf("default comments %q", c.Comments)
	}
	item := v + "/contents/" + c.ID
	var first content.Comment
	s.call(http.StatusCreated, u1, "POST", item+"/comments", content.CommentInput{Body: "first"}, &first)
	s.call(http.StatusOK, staff, "PATCH", item, catalog.ContentPatch{Comments: ptr(content.CommentsClosed)}, &c)
	if c.Comments != content.CommentsClosed {
		t.Fatalf("patched comments %q", c.Comments)
	}
	rec := s.call(http.StatusForbidden, u1, "POST", item+"/comments", content.CommentInput{Body: "late"}, nil)
	if !strings.Contains(rec.Body.String(), `"code":"comments_closed"`) {
		t.Fatalf("closed: %s", rec.Body)
	}
	s.call(http.StatusForbidden, u1, "PATCH", v+"/comments/"+first.ID, content.CommentEdit{Body: "edited"}, nil)
	var list []content.Comment
	s.call(http.StatusOK, u1, "GET", item+"/comments", nil, &list)
	if len(list) != 1 {
		t.Fatalf("closed comments still show: %+v", list)
	}
	var standing content.CommentStanding
	s.call(http.StatusOK, u1, "GET", item+"/can-comment", nil, &standing)
	if standing.CanComment || standing.Comments != content.CommentsClosed {
		t.Fatalf("closed standing %+v", standing)
	}
	s.call(http.StatusOK, staff, "PATCH", item, catalog.ContentPatch{Comments: ptr(content.CommentsHidden)}, nil)
	s.call(http.StatusOK, u1, "GET", item+"/comments", nil, &list)
	if len(list) != 0 {
		t.Fatalf("hidden comments show: %+v", list)
	}
	var feed []content.FeedItem
	s.call(http.StatusOK, u1, "GET", v+"/comments/latest", nil, &feed)
	if len(feed) != 0 {
		t.Fatalf("hidden comments in the feed: %+v", feed)
	}
	s.call(http.StatusNotFound, u1, "POST", v+"/comments/"+first.ID+"/like", nil, nil)
	s.call(http.StatusBadRequest, staff, "PATCH", item, catalog.ContentPatch{Comments: ptr("sometimes")}, nil)
	s.call(http.StatusOK, staff, "PATCH", item, catalog.ContentPatch{Comments: ptr(content.CommentsOpen)}, nil)
	s.call(http.StatusCreated, u1, "POST", item+"/comments", content.CommentInput{Body: "open again"}, nil)

	// A site that excludes restricted content does not find it, directly
	// or through interactions; the main site does.
	r := create(catalog.ContentInput{Restrictions: []string{"restricted"}})
	main, safe := "http://example.com"+v+"/contents/"+r.ID, "http://safe.example"+v+"/contents/"+r.ID
	s.call(http.StatusOK, u1, "GET", main, nil, nil)
	s.call(http.StatusOK, u1, "POST", main+"/like", nil, nil)
	for _, call := range []struct{ method, path string }{{"GET", ""}, {"GET", "/comments"}, {"POST", "/like"}, {"GET", "/reaction"},
		{"POST", "/favorite"}, {"GET", "/can-comment"}} {
		s.call(http.StatusNotFound, u1, call.method, safe+call.path, nil, nil)
	}
	s.call(http.StatusNotFound, access.Actor{Anonymous: true, IP: "10.0.0.3"}, "GET", safe+"/favorite", nil, nil)
	s.call(http.StatusOK, u1, "GET", "http://safe.example"+item+"/comments", nil, nil)
}
