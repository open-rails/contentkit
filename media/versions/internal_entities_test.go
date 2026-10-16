package versions

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
)

func TestInternalEntityMediaAndDownloadMetadata(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, ctx, pool)
	cfg := kinds.Config{Languages: []string{"en"}, Kinds: map[string]kinds.Kind{
		"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}, Groups: []string{"tag"}},
	}, GroupKinds: map[string]kinds.GroupKind{"tag": {}}}
	store, err := groups.New(groups.Options{Pool: pool, Schema: schema, Tenant: "test", Kinds: cfg})
	if err != nil {
		t.Fatal(err)
	}
	entities, err := store.CreateGroups(ctx, []groups.GroupInput{
		{Kind: "tag", Slug: "public", Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Public"}}},
		{Kind: "tag", Slug: "internal", Internal: true, Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Private marker"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{o: Options{Pool: pool, Schema: schema, Tenant: "test", Kinds: cfg, Auth: testauth.Auth{Authz: testauth.All{}}},
		t: tables{groups: pgx.Identifier{schema, "content_groups"}.Sanitize()}}
	ref := contentref.New("test", "tag", string(entities[1].ID))
	staff := testauth.Actor(access.Actor{ID: "staff"}, testauth.All{})
	reader := testauth.Actor(access.Actor{ID: "reader"}, testauth.None{})
	for _, actor := range []access.Actor{{Anonymous: true}, reader, staff} {
		out := map[contentref.ContentKey]resolve.Resolution{}
		if err := s.resolveGroups(ctx, []contentref.ContentRef{ref}, actor, out); err != nil || out[ref.Key()].Visible {
			t.Fatalf("ordinary internal media visible to %+v: %+v, %v", actor, out, err)
		}
	}
	for _, actor := range []access.Actor{reader, staff} {
		out := map[contentref.ContentKey]resolve.Resolution{}
		if err := s.resolveGroups(resolve.WithEditorRead(ctx), []contentref.ContentRef{ref}, actor, out); err != nil {
			t.Fatal(err)
		}
		if out[ref.Key()].Visible != (actor.ID == "staff") {
			t.Fatalf("editor media permission for %s: %+v", actor.ID, out)
		}
	}
	grant, err := s.canEditGroup(ctx, staff, media.UploadTarget{Ref: ref})
	if err != nil || !grant.Allowed {
		t.Fatalf("staff cannot edit internal media: %+v, %v", grant, err)
	}
	contentID, versionID := contentref.NewID(), contentref.NewID()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO " + pgx.Identifier{schema, "content_items"}.Sanitize() + " (tenant_id,id,kind) VALUES ('test',$1,'gallery')", []any{contentID}},
		{"INSERT INTO " + pgx.Identifier{schema, "content_versions"}.Sanitize() + " (tenant_id,id,content_id,languages) VALUES ('test',$2,$1,ARRAY['en'])", []any{contentID, versionID}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AddMemberships(ctx, []groups.Membership{{GroupID: entities[0].ID, ContentID: contentID}, {GroupID: entities[1].ID, ContentID: contentID}}); err != nil {
		t.Fatal(err)
	}
	metadata, err := s.downloadGroups(ctx, version{Target: Target{Content: contentID, Version: versionID}, Language: "en"})
	if err != nil || len(metadata) != 1 || metadata[0].Slug != "public" {
		t.Fatalf("download metadata leaked internal entity: %+v, %v", metadata, err)
	}
}
