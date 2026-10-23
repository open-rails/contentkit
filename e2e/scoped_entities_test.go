package e2e

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
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
	var made []entities.Entity
	f.want(http.StatusCreated, staff, "POST", "/entities", []entities.EntityInput{
		{Kind: "series", Slug: "before"}, {Kind: "series", Slug: "after"},
		{Kind: "tag", Slug: "edition-a"}, {Kind: "tag", Slug: "edition-b"}, {Kind: "tag", Slug: "replacement"},
	}, &made)
	before, after, first, sibling, replacement := made[0].ID, made[1].ID, made[2].ID, made[3].ID, made[4].ID
	path := "/contents/" + c.ID + "/entities"
	f.want(http.StatusOK, staff, "PUT", path, entities.ContentEntities{Memberships: []entities.Membership{
		{EntityID: before}, {EntityID: first}, {EntityID: first, VersionID: a}, {EntityID: sibling, VersionID: b},
	}}, nil)
	sharedPath := path + "/shared"
	versionPath := "/contents/" + c.ID + "/versions/" + a + "/entities"
	shared := entities.ContentEntities{Kinds: []string{"series"}, Memberships: []entities.Membership{{EntityID: after}}}
	version := entities.ContentEntities{Kinds: []string{"tag"}, Memberships: []entities.Membership{{EntityID: replacement}}}
	for _, route := range []struct {
		path string
		body entities.ContentEntities
	}{{sharedPath, shared}, {versionPath, version}} {
		f.want(http.StatusUnauthorized, "", "PUT", route.path, route.body, nil)
		f.want(http.StatusForbidden, user, "PUT", route.path, route.body, nil)
	}
	type member struct {
		group   entities.EntityID
		version string
	}
	check := func(got entities.ContentMemberships, want map[member]bool) {
		t.Helper()
		if len(got.Memberships) != len(want) {
			t.Fatalf("scoped edit changed another scope: %+v", got.Memberships)
		}
		seen := map[member]bool{}
		for _, m := range got.Memberships {
			key := member{m.EntityID, m.VersionID}
			if m.ContentID != c.ID || !want[key] || seen[key] {
				t.Fatalf("unexpected scoped membership: %+v; want %+v", m, want)
			}
			seen[key] = true
		}
	}
	read := func(method, path string, body any) entities.ContentMemberships {
		t.Helper()
		// Omitted version_id means shared. Decoding over a prior response's
		// slice can retain its old struct fields when row order changes.
		var got entities.ContentMemberships
		f.want(http.StatusOK, staff, method, path, body, &got)
		return got
	}
	publicScopes := func(wantA, wantB []entities.EntityID) {
		t.Helper()
		var detail catalog.Content
		f.want(http.StatusOK, "", "GET", "/contents/"+c.ID, nil, &detail)
		entities := func(got []catalog.EntitySummary, want []entities.EntityID) {
			t.Helper()
			seen := map[entities.EntityID]bool{}
			for _, g := range got {
				seen[entities.EntityID(g.ID)] = true
			}
			if len(got) != len(want) || len(seen) != len(want) {
				t.Fatalf("entity scope: got %+v, want %v", got, want)
			}
			for _, id := range want {
				if !seen[id] {
					t.Fatalf("entity scope is missing %s: %+v", id, got)
				}
			}
		}
		entities(detail.Entities, []entities.EntityID{after, first})
		if len(detail.Versions) != 2 {
			t.Fatalf("visible versions: %+v", detail.Versions)
		}
		for _, version := range detail.Versions {
			switch version.ID {
			case a:
				entities(version.Entities, wantA)
			case b:
				entities(version.Entities, wantB)
			default:
				t.Fatalf("unexpected version %s", version.ID)
			}
		}
	}
	got := read("PUT", sharedPath, shared)
	check(got, map[member]bool{{after, ""}: true, {first, ""}: true, {first, a}: true, {sibling, b}: true})
	f.want(http.StatusOK, staff, "POST", "/contents/"+c.ID+"/publish", catalog.PublishInput{}, nil)
	// A real legacy tag attached to both parent and A remains in both scopes.
	publicScopes([]entities.EntityID{first}, []entities.EntityID{sibling})
	// The route supplies the version ID; callers do not need to repeat it in
	// every membership, and may not use it to write a sibling instead.
	got = read("PUT", versionPath, version)
	check(got, map[member]bool{{after, ""}: true, {first, ""}: true, {replacement, a}: true, {sibling, b}: true})
	publicScopes([]entities.EntityID{replacement}, []entities.EntityID{sibling})
	f.want(http.StatusBadRequest, staff, "PUT", versionPath, entities.ContentEntities{Kinds: []string{"tag"},
		Memberships: []entities.Membership{{EntityID: first, VersionID: b}}}, nil)
	f.want(http.StatusNotFound, staff, "PUT", "/contents/"+c.ID+"/versions/"+uuid.Must(uuid.NewV7()).String()+"/entities",
		entities.ContentEntities{Kinds: []string{"tag"}}, nil)
	got = read("GET", path, nil)
	check(got, map[member]bool{{after, ""}: true, {first, ""}: true, {replacement, a}: true, {sibling, b}: true})
	got = read("PUT", "/contents/"+c.ID+"/versions/"+b+"/entities", entities.ContentEntities{Kinds: []string{"tag"}})
	check(got, map[member]bool{{after, ""}: true, {first, ""}: true, {replacement, a}: true})
	publicScopes([]entities.EntityID{replacement}, nil)
}
