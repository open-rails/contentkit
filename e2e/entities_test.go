package e2e

import (
	"net/http"
	"testing"

	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/contentkit/groups"
)

// Administration can inspect internal classifications, but signing in as staff
// does not turn a public entity route into a management route.
func TestEntityManagementKeepsInternalClassificationsPrivate(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	_, user := w.person(iam.Role{})
	f := serve(t, w.engine(nil))
	var created []groups.Group
	f.want(http.StatusCreated, staff, "POST", "/entities", []groups.GroupInput{
		{Kind: "tag", Slug: "visible", Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Visible"}}},
		{Kind: "tag", Slug: "internal-marker", Internal: true, Names: []groups.Name{
			{Language: "en", Kind: groups.NameCanonical, Name: "Internal marker"},
			{Language: "en", Kind: groups.NameAlias, Name: "Private classification"},
			{Language: "ja", Kind: groups.NameCanonical, Name: "内部分類"},
		}},
	}, &created)
	visible, internal := created[0], created[1]
	manageDetail := "/manage/entities/" + string(internal.ID)
	for _, path := range []string{"/manage/entities?kind=tag", manageDetail} {
		f.want(http.StatusUnauthorized, "", "GET", path, nil, nil)
		f.want(http.StatusForbidden, user, "GET", path, nil, nil)
	}

	var page groups.GroupPage
	f.want(http.StatusOK, staff, "GET", "/manage/entities?kind=tag&q=classification&limit=1", nil, &page)
	if len(page.Groups) != 1 || page.Groups[0].ID != internal.ID || !page.Groups[0].Internal || page.NextCursor != "" {
		t.Fatalf("management alias lookup: %+v", page)
	}
	f.want(http.StatusOK, staff, "GET", "/manage/entities?kind=tag&sort=name&limit=1", nil, &page)
	if len(page.Groups) != 1 || page.Groups[0].ID != internal.ID || page.NextCursor == "" {
		t.Fatalf("management pagination omitted internal classification: %+v", page)
	}
	var detail groups.GroupDetail
	f.want(http.StatusOK, staff, "GET", manageDetail+"?language=ja", nil, &detail)
	if detail.ID != internal.ID || !detail.Internal || detail.Name != "内部分類" || detail.NameLanguage != "ja" || len(detail.Names) != 3 {
		t.Fatalf("localized management detail: %+v", detail)
	}

	for _, actor := range []string{"", user, staff} {
		page = groups.GroupPage{}
		f.want(http.StatusOK, actor, "GET", "/entities?kind=tag&sort=name&limit=1&include_internal=true", nil, &page)
		if len(page.Groups) != 1 || page.Groups[0].ID != visible.ID || page.Groups[0].Internal || page.NextCursor != "" {
			t.Fatalf("public list exposed internal rows or pagination for actor %q: %+v", actor, page)
		}
		f.want(http.StatusNotFound, actor, "GET", "/entities/"+string(internal.ID), nil, nil)
	}
	f.want(http.StatusForbidden, user, "PATCH", "/entities/"+string(internal.ID), groups.GroupUpdate{Internal: ptr(false)}, nil)
	f.want(http.StatusOK, staff, "PATCH", "/entities/"+string(internal.ID), groups.GroupUpdate{Internal: ptr(false)}, nil)
	detail = groups.GroupDetail{}
	f.want(http.StatusOK, "", "GET", "/entities/"+string(internal.ID), nil, &detail)
	if detail.Internal || detail.Name != "Internal marker" {
		t.Fatalf("explicitly published classification remains hidden: %+v", detail)
	}
}
