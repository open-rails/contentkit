package e2e

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/importer"
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
