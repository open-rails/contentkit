package entities

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/testauth"
)

func TestInternalEntityVisibilityAndIndexLifecycle(t *testing.T) {
	f := newFixture(t)
	f.content("gallery", 1, live("en"))
	made := f.create(
		EntityInput{Kind: "tag", Slug: "public", Names: []Name{canonical("en", "Public")}},
		EntityInput{Kind: "tag", Slug: "restricted", Internal: true, Names: []Name{canonical("en", "Internal restriction"), alias("en", "Secret marker")}},
	)
	public, internal := made[0], made[1]
	if !internal.Internal {
		t.Fatal("internal flag was not stored")
	}
	f.add(Membership{EntityID: internal.ID, ContentID: cid(1)})
	if !slices.Contains(f.facetEntities()[vid(1, 0)], string(internal.ID)) {
		t.Fatal("policy marker is missing from effective access facets")
	}
	if err := f.s.AddEdges(f.ctx, []Edge{{From: public.ID, Relation: "related", To: internal.ID}}); err != nil {
		t.Fatal(err)
	}
	page, err := f.s.List(f.ctx, ListOptions{Kind: "tag", Limit: 1})
	if err != nil || len(page.Entities) != 1 || page.Entities[0].ID != public.ID || page.NextCursor != "" {
		t.Fatalf("public list leaked internal entity: %+v, %v", page, err)
	}
	page, err = f.s.List(f.ctx, ListOptions{Kind: "tag", Query: "Secret", IncludeInternal: true})
	if err != nil || len(page.Entities) != 1 || page.Entities[0].ID != internal.ID || !page.Entities[0].Internal {
		t.Fatalf("management alias lookup: %+v, %v", page, err)
	}
	cards, err := f.s.Cards(f.ctx, []EntityID{public.ID, internal.ID}, CardOptions{})
	if err != nil || len(cards) != 1 || cards[0].ID != public.ID {
		t.Fatalf("public cards leaked internal entity: %+v, %v", cards, err)
	}
	if _, err := f.s.Detail(f.ctx, internal.ID, "en"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("public direct entity access: %v", err)
	}
	detail, err := f.s.Detail(f.ctx, public.ID, "en")
	if err != nil || len(detail.Edges) != 0 {
		t.Fatalf("public relationship leaked internal marker: %+v, %v", detail, err)
	}
	detail, err = f.s.ManagementDetail(f.ctx, internal.ID, "en")
	if err != nil || !detail.Internal || len(detail.Edges) != 1 {
		t.Fatalf("management detail: %+v, %v", detail, err)
	}
	memberships, err := f.s.contentMemberships(f.ctx, cid(1), "en")
	if err != nil || len(memberships.Entities) != 1 || !memberships.Entities[0].Internal {
		t.Fatalf("management memberships lost internal marker: %+v, %v", memberships, err)
	}
	ref := contentref.New(tenant, "tag", string(internal.ID))
	if docs, err := f.s.BuildDocuments(f.ctx, "tag", "en", []contentref.ContentRef{ref}); err != nil || len(docs) != 0 {
		t.Fatalf("internal entity indexed: %+v, %v", docs, err)
	}
	if docs, _, _, err := f.s.ListDocuments(f.ctx, "tag", "en", "", 100); err != nil || len(docs) != 1 || docs[0].ContentID != string(public.ID) {
		t.Fatalf("internal entity reindexed: %+v, %v", docs, err)
	}
	var deleted bool
	if err := f.pool.QueryRow(f.ctx, "SELECT is_deleted FROM "+f.s.table("content_search_dirty")+" WHERE content_id=$1 AND language='en'", string(internal.ID)).Scan(&deleted); err != nil || !deleted {
		t.Fatalf("internal search deletion was not queued: %v, %v", deleted, err)
	}
	f.changed = nil
	if _, err := f.s.UpdateEntities(f.ctx, []EntityUpdate{{ID: internal.ID, Internal: ptr(false)}}); err != nil {
		t.Fatal(err)
	}
	if len(f.changed) != 1 || !slices.Contains(f.changed[0], cid(1)) {
		t.Fatalf("visibility change did not refresh attached content indexes: %+v", f.changed)
	}
	if docs, err := f.s.BuildDocuments(f.ctx, "tag", "en", []contentref.ContentRef{ref}); err != nil || len(docs) != 1 {
		t.Fatalf("making entity public did not restore its document: %+v, %v", docs, err)
	}
	if _, err := f.s.UpdateEntities(f.ctx, []EntityUpdate{{ID: internal.ID, Internal: ptr(true)}}); err != nil {
		t.Fatal(err)
	}
	if docs, err := f.s.BuildDocuments(f.ctx, "tag", "en", []contentref.ContentRef{ref}); err != nil || len(docs) != 0 {
		t.Fatalf("making entity internal retained its document: %+v, %v", docs, err)
	}
}

func TestInternalEntityManagementAuthorization(t *testing.T) {
	f := newFixture(t)
	marker := f.create(EntityInput{Kind: "tag", Slug: "restricted", Internal: true, Names: []Name{canonical("en", "Restricted")}})[0]
	for _, tc := range []struct {
		name string
		auth testauth.Auth
		want int
	}{
		{"staff", testauth.Auth{Authz: testauth.All{}}, http.StatusOK},
		{"non-staff", testauth.Auth{Authz: testauth.None{}}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := f.s.Handler(tc.auth)
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/manage/entities?q=Restricted", "/manage/entities/" + string(marker.ID)} {
				req := httptest.NewRequest("GET", path, nil)
				req.Header.Set("Authorization", "Bearer user:staff")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != tc.want {
					t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
				}
				if tc.want == http.StatusOK && !strings.Contains(rec.Body.String(), `"internal":true`) {
					t.Fatal("management response omitted internal marker")
				}
			}
			for _, path := range []string{"/entities?include_internal=true", "/entities/" + string(marker.ID)} {
				req := httptest.NewRequest("GET", path, nil)
				req.Header.Set("Authorization", "Bearer user:staff")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if strings.Contains(rec.Body.String(), string(marker.ID)) {
					t.Fatalf("public route leaked internal marker: %s", rec.Body)
				}
			}
		})
	}
	reader := f.s.As(testauth.Auth{Authz: testauth.None{}}, access.Actor{ID: "reader"})
	if _, err := reader.Cards(f.ctx, []EntityID{marker.ID}, CardOptions{IncludeInternal: true}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader opted in to internal cards: %v", err)
	}
	if _, err := reader.List(f.ctx, ListOptions{IncludeInternal: true}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader opted in to internal list: %v", err)
	}
	if _, err := reader.ManagementDetail(f.ctx, marker.ID, "en"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader accessed management detail: %v", err)
	}
	if _, err := reader.UpdateEntities(f.ctx, []EntityUpdate{{ID: marker.ID, Internal: ptr(false)}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader changed internal classification: %v", err)
	}
	if _, err := reader.CreateEntities(f.ctx, []EntityInput{{Kind: "channel", Slug: "hidden-channel", Internal: true}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader created internal entity: %v", err)
	}
}

func TestInternalEntityReferencesAndImports(t *testing.T) {
	f := newFixture(t)
	parent := f.create(EntityInput{Kind: "series", Slug: "internal-series", Internal: true})[0]
	child := f.create(EntityInput{Kind: "series", Slug: "child", ParentID: parent.ID})[0]
	character := f.create(EntityInput{Kind: "character", Slug: "character", ScopeID: parent.ID})[0]
	for _, entity := range []Entity{child, character} {
		detail, err := f.s.Detail(f.ctx, entity.ID, "en")
		if err != nil || detail.ParentID != "" || detail.ScopeID != "" || detail.Parent != nil || detail.Scope != nil {
			t.Fatalf("public entity revealed internal relationship: %+v, %v", detail, err)
		}
	}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if err := f.s.ImportEntities(f.ctx, tx, []ImportEntity{{ID: child.ID, Kind: "series", Slug: child.Slug, ParentID: parent.ID, Internal: ptr(true)}}, false); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Detail(f.ctx, child.ID, "en"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("imported internal entity still public: %v", err)
	}
	detail, err := f.s.ManagementDetail(f.ctx, child.ID, "en")
	if err != nil || !detail.Internal || detail.ParentID != parent.ID {
		t.Fatalf("import lost classification or parent: %+v, %v", detail, err)
	}
}

func TestInternalEntityMergeRequiresMatchingClassification(t *testing.T) {
	f := newFixture(t)
	entities := f.create(
		EntityInput{Kind: "tag", Slug: "public", Names: []Name{canonical("en", "Public")}},
		EntityInput{Kind: "tag", Slug: "internal", Internal: true, Names: []Name{canonical("en", "Private marker")}},
	)
	for _, merge := range []Merge{{From: entities[0].ID, Into: entities[1].ID}, {From: entities[1].ID, Into: entities[0].ID}} {
		if _, err := f.s.Merge(f.ctx, []Merge{merge}); !errors.Is(err, ErrConflict) {
			t.Fatalf("mixed classification merge should fail: %v", err)
		}
	}
	public, err := f.s.Detail(f.ctx, entities[0].ID, "en")
	if err != nil || len(public.Names) != 1 || public.Names[0].Name != "Public" {
		t.Fatalf("rejected merge leaked aliases: %+v, %v", public, err)
	}
	if _, err := f.s.UpdateEntities(f.ctx, []EntityUpdate{{ID: entities[0].ID, Internal: ptr(true)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Merge(f.ctx, []Merge{{From: entities[1].ID, Into: entities[0].ID}}); err != nil {
		t.Fatalf("explicitly matching classification cannot merge: %v", err)
	}
}
