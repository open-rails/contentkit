package e2e

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/groups"
)

// Shared entity edits preserve both editions, and an edition edit preserves
// the parent's associations and its sibling. These routes use the real
// AuthKit actor and the same HTTP contracts as an embedded admin editor.
func TestScopedEntityEditsPreserveParentAndSibling(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	_, user := w.person(iam.Role{})
	f := serve(t, w.engine(nil))
	var c catalog.Content
	f.want(http.StatusCreated, staff, "POST", "/contents", catalog.ContentInput{Kind: "gallery",
		Text:     map[string]catalog.Text{"en": {Title: "Scoped editions"}},
		Versions: []catalog.VersionInput{{Languages: []string{"en"}}, {Languages: []string{"ja"}}}}, &c)
	a, b := c.Versions[0].ID, c.Versions[1].ID
	var made []groups.Group
	f.want(http.StatusCreated, staff, "POST", "/entities", []groups.GroupInput{
		{Kind: "series", Slug: "before"}, {Kind: "series", Slug: "after"},
		{Kind: "tag", Slug: "edition-a"}, {Kind: "tag", Slug: "edition-b"}, {Kind: "tag", Slug: "replacement"},
	}, &made)
	before, after, first, sibling, replacement := made[0].ID, made[1].ID, made[2].ID, made[3].ID, made[4].ID
	path := "/contents/" + c.ID + "/entities"
	f.want(http.StatusOK, staff, "PUT", path, groups.ContentGroups{Memberships: []groups.Membership{
		{GroupID: before}, {GroupID: first, VersionID: a}, {GroupID: sibling, VersionID: b},
	}}, nil)
	sharedPath := path + "/shared"
	versionPath := "/contents/" + c.ID + "/versions/" + a + "/entities"
	shared := groups.ContentGroups{Kinds: []string{"series"}, Memberships: []groups.Membership{{GroupID: after}}}
	version := groups.ContentGroups{Kinds: []string{"tag"}, Memberships: []groups.Membership{{GroupID: replacement}}}
	for _, route := range []struct {
		path string
		body groups.ContentGroups
	}{{sharedPath, shared}, {versionPath, version}} {
		f.want(http.StatusUnauthorized, "", "PUT", route.path, route.body, nil)
		f.want(http.StatusForbidden, user, "PUT", route.path, route.body, nil)
	}
	check := func(got groups.ContentMemberships, want map[groups.GroupID]string) {
		t.Helper()
		if len(got.Memberships) != len(want) {
			t.Fatalf("scoped edit changed another scope: %+v", got.Memberships)
		}
		for _, m := range got.Memberships {
			version, ok := want[m.GroupID]
			if !ok || m.ContentID != c.ID || m.VersionID != version {
				t.Fatalf("unexpected scoped membership: %+v", m)
			}
		}
	}
	var got groups.ContentMemberships
	f.want(http.StatusOK, staff, "PUT", sharedPath, shared, &got)
	check(got, map[groups.GroupID]string{after: "", first: a, sibling: b})
	// The route supplies the version ID; callers do not need to repeat it in
	// every membership, and may not use it to write a sibling instead.
	f.want(http.StatusOK, staff, "PUT", versionPath, version, &got)
	check(got, map[groups.GroupID]string{after: "", replacement: a, sibling: b})
	f.want(http.StatusBadRequest, staff, "PUT", versionPath, groups.ContentGroups{Kinds: []string{"tag"},
		Memberships: []groups.Membership{{GroupID: first, VersionID: b}}}, nil)
	f.want(http.StatusNotFound, staff, "PUT", "/contents/"+c.ID+"/versions/"+uuid.Must(uuid.NewV7()).String()+"/entities",
		groups.ContentGroups{Kinds: []string{"tag"}}, nil)
	f.want(http.StatusOK, staff, "GET", path, nil, &got)
	check(got, map[groups.GroupID]string{after: "", replacement: a, sibling: b})
	f.want(http.StatusOK, staff, "PUT", "/contents/"+c.ID+"/versions/"+b+"/entities",
		groups.ContentGroups{Kinds: []string{"tag"}}, &got)
	check(got, map[groups.GroupID]string{after: "", replacement: a})
}
