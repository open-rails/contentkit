package catalog

import (
	"slices"
	"testing"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/entities"
)

func TestKeywordEntitiesUseTimelessAssociations(t *testing.T) {
	f := newFixture(t, nil)
	cfg := config()
	kind := cfg.EntityKinds["tag"]
	kind.Windows = true
	cfg.EntityKinds["tag"] = kind
	var err error
	f.entities, err = entities.New(entities.Options{Pool: f.pool, Schema: f.schema, Tenant: tenant, Kinds: cfg})
	if err != nil {
		t.Fatal(err)
	}
	static, dated, parent := f.entity("tag", "static"), f.entity("tag", "dated"), f.entity("tag", "parent")
	parentID := entities.EntityID(parent)
	if _, err := f.entities.UpdateEntities(f.ctx, []entities.EntityUpdate{{ID: entities.EntityID(static), ParentID: &parentID}}); err != nil {
		t.Fatal(err)
	}
	id := f.create("staff", gallery("Discussion", "en"))
	f.publish("staff", id, nil)
	f.join(static, id, "")
	f.join(dated, id, "")
	refs := []contentref.ContentRef{f.s.Ref("gallery", id)}
	check := func(want ...string) {
		t.Helper()
		docs, err := f.s.BuildDocuments(f.ctx, "gallery", "en", refs)
		if err != nil || len(docs) != 1 {
			t.Fatalf("documents: %+v %v", docs, err)
		}
		slices.Sort(docs[0].Keywords)
		slices.Sort(want)
		if !slices.Equal(docs[0].Keywords, want) {
			t.Fatalf("keywords=%v want=%v", docs[0].Keywords, want)
		}
	}
	// Enabling windows on a kind does not discard its static associations.
	check("Static", "Dated", "Parent")
	if _, err := f.pool.Exec(f.ctx, `UPDATE `+f.table("content_memberships")+` SET active_from=now()-interval '3 days' WHERE entity_id=$1`, dated); err != nil {
		t.Fatal(err)
	}
	check("Static", "Parent")
	if _, err := f.pool.Exec(f.ctx, `UPDATE `+f.table("content_memberships")+` SET active_until=now()-interval '2 days' WHERE entity_id=$1`, dated); err != nil {
		t.Fatal(err)
	}
	check("Static", "Parent")
}
