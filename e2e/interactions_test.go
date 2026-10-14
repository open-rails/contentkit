package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/media"
)

// A group imported by staff takes reactions, favorites and comments like
// content; staff ban words and addresses from comments; a reader's recent
// searches list and clear.
func TestGroupInteractionsAndModeration(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	_, user := w.person(iam.Role{})
	f := serve(t, w.engine(nil))

	var res importer.Result
	f.want(http.StatusOK, staff, "POST", "/import", importer.Batch{Source: "e2e",
		Groups: []importer.GroupRecord{{ExternalID: "t1", Kind: "tag", Slug: "imported"}}}, &res)
	id := res.IDs["group"]["t1"]
	if id == "" || len(res.Failed) > 0 {
		t.Fatalf("import %+v", res)
	}
	g := "/groups/" + id

	var rc content.ReactionCounts
	f.want(http.StatusOK, user, "POST", g+"/like", nil, &rc)
	f.want(http.StatusOK, user, "POST", g+"/dislike", nil, &rc)
	if rc.Likes != 0 || rc.Dislikes != 1 || rc.Mine != -1 {
		t.Fatalf("after dislike %+v", rc)
	}
	var mine discovery.InteractionPage
	f.want(http.StatusOK, user, "GET", "/me/reactions?target=groups", nil, &mine)
	if len(mine.Items) != 1 || mine.Items[0].Group == nil || string(mine.Items[0].Group.ID) != id || mine.Items[0].Value != -1 {
		t.Fatalf("my reactions %+v", mine)
	}
	f.want(http.StatusOK, user, "DELETE", g+"/reaction", nil, &rc)
	f.want(http.StatusOK, "", "GET", g+"/reaction", nil, &rc)
	if rc.Likes != 0 || rc.Dislikes != 0 {
		t.Fatalf("after clearing %+v", rc)
	}

	var fav content.FavoriteState
	f.want(http.StatusOK, user, "POST", g+"/favorite", nil, &fav)
	f.want(http.StatusOK, user, "GET", g+"/favorite", nil, &fav)
	if !fav.Favorited || fav.Count != 1 {
		t.Fatalf("favorite %+v", fav)
	}
	f.want(http.StatusOK, user, "DELETE", g+"/favorite", nil, &fav)
	if fav.Favorited || fav.Count != 0 {
		t.Fatalf("unfavorite %+v", fav)
	}

	var standing content.CommentStanding
	f.want(http.StatusOK, user, "GET", g+"/can-comment", nil, &standing)
	if !standing.CanComment {
		t.Fatalf("standing %+v", standing)
	}
	f.want(http.StatusCreated, user, "POST", g+"/comments", content.CommentInput{Body: "nice tag"}, nil)

	// A rejected word refuses the comment; an address ban refuses the
	// commenter (httptest's peer is 192.0.2.1).
	var words []content.BannedWord
	f.want(http.StatusOK, staff, "POST", "/moderation/banned-words", []content.BannedWordInput{{Word: "frobnicate", Action: content.WordReject}}, &words)
	f.want(http.StatusOK, staff, "GET", "/moderation/banned-words", nil, &words)
	if len(words) != 1 || words[0].Word != "frobnicate" {
		t.Fatalf("banned words %+v", words)
	}
	f.want(http.StatusUnprocessableEntity, user, "POST", g+"/comments", content.CommentInput{Body: "frobnicate it"}, nil)
	f.want(http.StatusNoContent, staff, "DELETE", "/moderation/banned-words", []string{"frobnicate"}, nil)

	var bans []content.IPBan
	f.want(http.StatusOK, staff, "POST", "/moderation/ip-bans", []content.IPBanInput{{Network: "192.0.2.0/24", Reason: "spam"}}, &bans)
	f.want(http.StatusOK, staff, "GET", "/moderation/ip-bans", nil, &bans)
	if len(bans) != 1 || bans[0].Network != "192.0.2.0/24" {
		t.Fatalf("ip bans %+v", bans)
	}
	f.want(http.StatusForbidden, user, "POST", g+"/comments", content.CommentInput{Body: "again"}, nil)
	f.want(http.StatusNoContent, staff, "DELETE", "/moderation/ip-bans", []string{"192.0.2.0/24"}, nil)

	var comments []content.Comment
	f.want(http.StatusOK, "", "GET", g+"/comments", nil, &comments)
	if len(comments) != 1 || comments[0].Body != "nice tag" {
		t.Fatalf("group comments %+v", comments)
	}

	var recent discovery.RecentSearches
	f.want(http.StatusOK, user, "GET", "/search?q="+url.QueryEscape("imported tag"), nil, nil)
	f.want(http.StatusOK, user, "GET", "/me/searches", nil, &recent)
	if len(recent.Searches) != 1 || recent.Searches[0].Query != "imported tag" {
		t.Fatalf("recent searches %+v", recent)
	}
	f.want(http.StatusNoContent, user, "DELETE", "/me/searches", nil, nil)
	f.want(http.StatusOK, user, "GET", "/me/searches", nil, &recent)
	if len(recent.Searches) != 0 {
		t.Fatalf("after clearing %+v", recent)
	}
}

func TestImportedExternalIDPagination(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	_, user := w.person(iam.Role{})
	f := serve(t, w.engine(nil))
	var imported importer.Result
	f.want(http.StatusOK, staff, "POST", "/import", importer.Batch{Source: "e2e-external",
		Groups: []importer.GroupRecord{
			{ExternalID: "tag:1", Kind: "tag", Slug: "first"},
			{ExternalID: "tag:2", Kind: "tag", Slug: "second"},
			{ExternalID: "tag:3", Kind: "tag", Slug: "third"},
		}}, &imported)
	if len(imported.Failed) > 0 || len(imported.IDs["group"]) != 3 {
		t.Fatalf("import: %+v", imported)
	}
	q := importer.ExternalIDsQuery{Source: "e2e-external", Lists: []importer.ExternalIDList{{Entity: importer.EntityGroup, Limit: 1}}}
	f.want(http.StatusUnauthorized, "", "POST", "/import/external-ids", q, nil)
	f.want(http.StatusForbidden, user, "POST", "/import/external-ids", q, nil)
	for n := 1; n <= 3; n++ {
		var page importer.ExternalIDs
		f.want(http.StatusOK, staff, "POST", "/import/external-ids", q, &page)
		if len(page.Lists) != 1 || len(page.Lists[0].IDs) != 1 {
			t.Fatalf("page %d: %+v", n, page)
		}
		id := page.Lists[0].IDs[0]
		want := fmt.Sprintf("tag:%d", n)
		if page.Lists[0].Entity != importer.EntityGroup || id.ExternalID != want || id.ID != imported.IDs["group"][want] || id.Deleted {
			t.Fatalf("page %d: %+v", n, page)
		}
		q.Lists[0].Cursor = page.Lists[0].NextCursor
		if (q.Lists[0].Cursor == "") != (n == 3) {
			t.Fatalf("page %d cursor: %q", n, q.Lists[0].Cursor)
		}
	}
	q.Source = "another-source"
	var empty importer.ExternalIDs
	f.want(http.StatusOK, staff, "POST", "/import/external-ids", q, &empty)
	if len(empty.Lists) != 1 || len(empty.Lists[0].IDs) != 0 || empty.Lists[0].NextCursor != "" {
		t.Fatalf("another source's ids: %+v", empty)
	}
}

// A group shows in the typeahead a moment after it is made, well before the
// next periodic sync: the write queues its documents' sync.
func TestTypeaheadFollowsGroupWrites(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	f := serve(t, w.engine(nil))
	shows := func(within time.Duration) bool {
		name := "Nebula " + uuid.NewString()[:8]
		var made []groups.Group
		f.want(http.StatusCreated, staff, "POST", "/groups", []groups.GroupInput{{Kind: "tag", Slug: strings.ReplaceAll(strings.ToLower(name), " ", "-"),
			Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: name}}}}, &made)
		for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			var got discovery.Typeahead
			f.want(http.StatusOK, "", "GET", "/typeahead?group_kinds=tag&q="+url.QueryEscape(name), nil, &got)
			if len(got.Groups) == 1 && got.Groups[0].ID == made[0].ID {
				return true
			}
		}
		return false
	}
	// The first may wait for the sync the engine runs on start.
	if !shows(time.Minute) || !shows(5*time.Second) {
		t.Fatal("a new group is not in the typeahead")
	}
}

// Staff importing stage a server-side copy of a file in the bucket outside
// ContentKit's namespaces and commit it; a person may not.
func TestImportStagesBucketCopies(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	_, user := w.person(iam.Role{})
	f := serve(t, w.engine(nil))
	album, _ := w.published(f, staff, "album")
	src, body := "legacy-"+w.tenant+"/0001.png", pngBytes(t, 16)
	if _, err := w.env.Store.Put(t.Context(), src, bytes.NewReader(body), int64(len(body)), media.PutOptions{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.env.Store.Delete(context.Background(), src) })
	ref := media.RefBody{Kind: "album", ID: album}
	in := media.CopyBody{Ref: ref, Files: []media.CopyRequest{{Path: "originals/0001", Type: "image/png", From: src, Size: int64(len(body))}}}
	f.want(http.StatusForbidden, user, "POST", "/media/upload/copy", in, nil)
	var staged media.CopyReply
	f.want(http.StatusOK, staff, "POST", "/media/upload/copy", in, &staged)
	if len(staged.Files) != 1 || staged.Files[0].Path != "originals/0001.png" || staged.Files[0].Size != int64(len(body)) {
		t.Fatalf("staged %+v", staged)
	}
	var done media.CommitReply
	f.want(http.StatusOK, staff, "POST", "/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
		Ops: []media.Op{{Op: media.OpPut, Path: staged.Files[0].Path, Blob: staged.Files[0].Blob}}}, &done)
	if len(done.Files) != 1 || done.Files[0].Path != "originals/0001.png" {
		t.Fatalf("committed %+v", done.Files)
	}
}

// An importer's legacy identifiers resolve over the API as codes do,
// following merges: hidden content is not_found, removed content gone. The
// imported comments setting holds, and the legacy view counts show on cards
// and order most_viewed.
func TestImportedLegacyURLsCommentsAndViews(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	_, user := w.person(iam.Role{})
	f := serve(t, w.engine(nil))
	t0 := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	gallery := func(n int, state, comments string, views int64) importer.ContentRecord {
		ext := fmt.Sprintf("gallery:%d", n)
		return importer.ContentRecord{ExternalID: ext, Kind: "gallery", State: state, PublishedAt: &t0, Comments: comments, ViewBaseline: views,
			Text:     map[string]catalog.Text{"en": {Title: fmt.Sprintf("Legacy %d", n)}},
			Versions: []importer.VersionRecord{{ExternalID: "version:" + ext, Languages: []string{"en"}, PublishedAt: &t0}}}
	}
	to := func(ext string) importer.TargetRef {
		return importer.TargetRef{Content: &importer.Ref{ExternalID: ext}}
	}
	tag := func(ext string) importer.TargetRef { return importer.TargetRef{Group: &importer.Ref{ExternalID: ext}} }
	var res importer.Result
	f.want(http.StatusOK, staff, "POST", "/import", importer.Batch{Source: "hentai0-legacy",
		Groups: []importer.GroupRecord{
			{ExternalID: "tag:1", Kind: "tag", Slug: "colour", Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Colour"}}},
			{ExternalID: "tag:2", Kind: "tag", Slug: "color", Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Color"}}}},
		Contents: []importer.ContentRecord{gallery(1, "published", "closed", 5000), gallery(2, "published", "", 50), gallery(3, "draft", "", 0),
			gallery(4, "deleted", "", 0)},
		Aliases: []importer.AliasRecord{{Kind: "video", Key: "101", Target: to("gallery:1")}, {Kind: "video", Key: "101/page", Locator: "3", Target: to("gallery:1")},
			{Kind: "video", Key: "103", Target: to("gallery:3")}, {Kind: "video", Key: "104", Target: to("gallery:4")},
			{Kind: "tag-name", Key: "Colour", Target: tag("tag:1")}}}, &res)
	if len(res.Failed) > 0 {
		t.Fatalf("import %+v", res.Failed)
	}
	g1, g2 := res.IDs["content"]["gallery:1"], res.IDs["content"]["gallery:2"]
	var color groups.GroupDetail
	f.want(http.StatusOK, staff, "POST", "/groups/"+res.IDs["group"]["tag:1"]+"/merge", groups.MergeInput{Into: groups.GroupID(res.IDs["group"]["tag:2"])}, nil)
	f.want(http.StatusOK, "", "GET", "/groups/"+res.IDs["group"]["tag:2"], nil, &color)

	key := func(kind, k string) contenturl.AliasKey {
		return contenturl.AliasKey{Source: "hentai0-legacy", Kind: kind, Key: k}
	}
	lookup := contenturl.AliasLookup{Aliases: []contenturl.AliasKey{key("video", "101"), key("video", "101/page"), key("video", "103"),
		key("video", "104"), key("video", "999"), key("tag-name", "Colour")}}
	var got contenturl.AliasResults
	f.want(http.StatusOK, "", "POST", "/codes/aliases?lang=en", lookup, &got)
	var item catalog.Content
	f.want(http.StatusOK, "", "GET", "/contents/"+g1, nil, &item)
	page := "/en/g/" + item.Code + "/legacy-1"
	for i, want := range []struct{ status, id, path, locator string }{
		{contenturl.AliasFound, g1, page, ""}, {contenturl.AliasFound, g1, page, "3"}, {contenturl.AliasNotFound, "", "", ""},
		{contenturl.AliasGone, "", "", ""}, {contenturl.AliasNotFound, "", "", ""}, {contenturl.AliasFound, string(color.ID), "/en/tags/" + color.Code + "/color", ""},
	} {
		a := got.Aliases[i]
		var id, path string
		if a.Resolved != nil {
			id, path = a.Resolved.ContentID, a.Resolved.Path
		}
		if a.AliasKey != lookup.Aliases[i] || a.Status != want.status || id != want.id || path != want.path || a.Locator != want.locator {
			t.Errorf("alias %s = %+v %q, want %+v", lookup.Aliases[i].Key, a, path, want)
		}
	}
	f.want(http.StatusBadRequest, "", "POST", "/codes/aliases", contenturl.AliasLookup{Aliases: []contenturl.AliasKey{key("Video", "1")}}, nil)

	if item.Comments != content.CommentsClosed || item.Views != 5000 {
		t.Fatalf("detail comments %q, views %d", item.Comments, item.Views)
	}
	f.want(http.StatusForbidden, user, "POST", "/contents/"+g1+"/comments", content.CommentInput{Body: "late"}, nil)
	f.want(http.StatusCreated, user, "POST", "/contents/"+g2+"/comments", content.CommentInput{Body: "open"}, nil)

	var cards discovery.ContentPage
	f.want(http.StatusOK, "", "GET", "/contents?kind=gallery&sort=most_viewed", nil, &cards)
	if len(cards.Contents) != 2 || cards.Contents[0].ID != g1 || cards.Contents[0].Counts.Views != 5000 || cards.Contents[1].Counts.Views != 50 {
		t.Fatalf("most viewed %+v", cards.Contents)
	}
}
