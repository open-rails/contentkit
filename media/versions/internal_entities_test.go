package versions

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/entities"
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
		"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}, Entities: []string{"tag"}},
	}, EntityKinds: map[string]kinds.EntityKind{"tag": {}}}
	store, err := entities.New(entities.Options{Pool: pool, Schema: schema, Tenant: "test", Kinds: cfg})
	if err != nil {
		t.Fatal(err)
	}
	entityRows, err := store.CreateEntities(ctx, []entities.EntityInput{
		{Kind: "tag", Slug: "public", Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Public"}}},
		{Kind: "tag", Slug: "internal", Internal: true, Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Private marker"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{o: Options{Pool: pool, Schema: schema, Tenant: "test", Kinds: cfg, Auth: testauth.Auth{Authz: testauth.All{}}},
		t: tables{entities: pgx.Identifier{schema, "content_entities"}.Sanitize()}}
	ref := contentref.New("test", "tag", string(entityRows[1].ID))
	staff := testauth.Actor(access.Actor{ID: "staff"}, testauth.All{})
	reader := testauth.Actor(access.Actor{ID: "reader"}, testauth.None{})
	for _, actor := range []access.Actor{{Anonymous: true}, reader, staff} {
		out := map[contentref.ContentKey]resolve.Resolution{}
		if err := s.resolveEntities(ctx, []contentref.ContentRef{ref}, actor, out); err != nil || out[ref.Key()].Visible {
			t.Fatalf("ordinary internal media visible to %+v: %+v, %v", actor, out, err)
		}
	}
	for _, actor := range []access.Actor{reader, staff} {
		out := map[contentref.ContentKey]resolve.Resolution{}
		if err := s.resolveEntities(resolve.WithEditorRead(ctx), []contentref.ContentRef{ref}, actor, out); err != nil {
			t.Fatal(err)
		}
		if out[ref.Key()].Visible != (actor.ID == "staff") {
			t.Fatalf("editor media permission for %s: %+v", actor.ID, out)
		}
	}
	grant, err := s.canEditEntity(ctx, staff, media.UploadTarget{Ref: ref})
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
	if err := store.AddMemberships(ctx, []entities.Membership{{EntityID: entityRows[0].ID, ContentID: contentID}, {EntityID: entityRows[1].ID, ContentID: contentID}}); err != nil {
		t.Fatal(err)
	}
	metadata, err := s.downloadEntities(ctx, version{Target: Target{Content: contentID, Version: versionID}, Language: "en"})
	if err != nil || len(metadata) != 1 || metadata[0].Slug != "public" {
		t.Fatalf("download metadata leaked internal entity: %+v, %v", metadata, err)
	}
}
