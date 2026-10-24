package e2e

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails/billing"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckauthkit "github.com/open-rails/contentkit/adapters/authkit"
	ckopenrails "github.com/open-rails/contentkit/adapters/openrails"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/entities"
)

// Channel access is granted and revoked directly in OpenRails. ContentKit
// applies the entitlement to the post and all of its comment interactions.
func TestMembersOnlyChannelComments(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	_, ownerToken := w.person(iam.Role{})
	member, memberToken := w.person(iam.Role{})
	_, strangerToken := w.person(iam.Role{})
	merchant := channelBilling(w)
	if _, err := merchant.UpdateCustomer(ctx, billing.CustomerID(uuid.MustParse(member.ID)), billing.UpdateCustomerParams{}); err != nil {
		t.Fatal(err)
	}
	e := w.engine(func(c *contentkit.Config, d *contentkit.Deps) {
		c.Content.EntityKinds["channel"] = channel
		c.Interactions.Anonymous = content.Anonymous{Comments: true, Reactions: true}
		d.Billing, d.Scopes = ckopenrails.Billing(merchant), ckauthkit.Scopes(w.ak)
	})
	f := serve(t, e)

	var made []entities.Entity
	f.want(http.StatusCreated, ownerToken, "POST", "/entities", []entities.EntityInput{{Kind: "channel", Slug: "club",
		Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Club"}}}}, &made)
	ch := string(made[0].ID)
	path := "/entities/" + ch

	var owned, seen entities.EntityDetail
	f.want(http.StatusOK, ownerToken, "GET", path, nil, &owned)
	if owned.Access == nil || !owned.Access.Editor {
		t.Fatalf("owner access: %+v", owned.Access)
	}
	f.want(http.StatusOK, strangerToken, "GET", path, nil, &seen)
	if seen.Access == nil || seen.Access.Member || seen.Access.Editor {
		t.Fatalf("stranger access: %+v", seen.Access)
	}
	_, granted := grantKeys(t, merchant, member.ID, owned.Access.Key.String())

	var post catalog.Content
	members := access.Rule{Level: access.EntityLevel, Entity: ch}
	f.want(http.StatusCreated, ownerToken, "POST", "/contents", catalog.ContentInput{Kind: "blog", OwnerEntityID: ch, Access: &members,
		Text: map[string]catalog.Text{"en": {Title: "Members night", Body: "for members"}}}, &post)
	f.want(http.StatusOK, ownerToken, "POST", "/contents/"+post.ID+"/publish", catalog.PublishInput{}, nil)
	thread := "/contents/" + post.ID + "/comments"

	var byOwner, byMember content.Comment
	f.want(http.StatusCreated, ownerToken, "POST", thread, content.CommentInput{Body: "welcome, members"}, &byOwner)
	f.want(http.StatusCreated, memberToken, "POST", thread, content.CommentInput{Body: "glad to be here", ReplyToID: byOwner.ID}, &byMember)
	f.want(http.StatusOK, memberToken, "POST", "/comments/"+byOwner.ID+"/like", nil, nil)

	bodies := func(token, target string) []string {
		t.Helper()
		var list []content.Comment
		f.want(http.StatusOK, token, "GET", target, nil, &list)
		out := []string{}
		for _, c := range list {
			out = append(out, c.Body)
		}
		return out
	}
	standing := func(token string) content.CommentStanding {
		t.Helper()
		var s content.CommentStanding
		f.want(http.StatusOK, token, "GET", "/contents/"+post.ID+"/can-comment", nil, &s)
		return s
	}
	latest := func(token string) string {
		t.Helper()
		return f.want(http.StatusOK, token, "GET", "/comments/latest?limit=100", nil, nil).Body.String()
	}
	for _, who := range []struct{ name, token string }{{"member", memberToken}, {"owner", ownerToken}} {
		if got := bodies(who.token, thread); !slices.Equal(got, []string{"welcome, members"}) {
			t.Fatalf("%s's thread %v", who.name, got)
		}
		if got := bodies(who.token, "/comments/"+byOwner.ID+"/replies"); !slices.Equal(got, []string{"glad to be here"}) {
			t.Fatalf("%s's replies %v", who.name, got)
		}
		if s := standing(who.token); s.Locked || !s.CanComment {
			t.Fatalf("%s's standing %+v", who.name, s)
		}
		if l := latest(who.token); !strings.Contains(l, "glad to be here") || !strings.Contains(l, "welcome, members") {
			t.Fatalf("%s's latest feed %s", who.name, l)
		}
	}

	for _, who := range []struct{ name, token string }{{"stranger", strangerToken}, {"anonymous", ""}} {
		if got := bodies(who.token, thread); len(got) != 0 {
			t.Fatalf("%s reads the thread %v", who.name, got)
		}
		if got := bodies(who.token, "/comments/"+byOwner.ID+"/replies"); len(got) != 0 {
			t.Fatalf("%s reads the replies %v", who.name, got)
		}
		if s := standing(who.token); !s.Locked || s.CanComment {
			t.Fatalf("%s's standing %+v", who.name, s)
		}
		if l := latest(who.token); strings.Contains(l, "glad to be here") || strings.Contains(l, "welcome, members") {
			t.Fatalf("%s's latest feed shows locked comments: %s", who.name, l)
		}
		for _, write := range []struct {
			method, target string
			body           any
		}{
			{"POST", thread, content.CommentInput{Body: "let me in", AnonName: "Visitor"}},
			{"POST", thread, content.CommentInput{Body: "me too", AnonName: "Visitor", ReplyToID: byOwner.ID}},
			{"POST", "/comments/" + byOwner.ID + "/like", nil},
		} {
			rec := f.call(who.token, write.method, write.target, write.body)
			var body struct{ Code string }
			if rec.Code != http.StatusForbidden || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Code != content.CodeCommentsLocked {
				t.Fatalf("%s %s %s: %d %s, want 403 comments_locked", who.name, write.method, write.target, rec.Code, rec.Body)
			}
		}
	}

	// Leaving locks the member out of the thread, and out of editing their own
	// comment there.
	if err := merchant.RevokeProductAccess(ctx, granted, billing.RevokeProductAccessParams{Reason: "fixture membership ended"}); err != nil {
		t.Fatal(err)
	}
	if got := bodies(memberToken, thread); len(got) != 0 {
		t.Fatalf("a former member reads the thread %v", got)
	}
	rec := f.call(memberToken, "PATCH", "/comments/"+byMember.ID, content.CommentEdit{Body: "edited"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"comments_locked"`) {
		t.Fatalf("a former member's edit: %d %s", rec.Code, rec.Body)
	}
}
