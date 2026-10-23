package catalog

import (
	"slices"
	"testing"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/groups"
)

func TestKeywordEntitiesUseTimelessAssociations(t *testing.T) {
	f := newFixture(t, nil)
	cfg := config()
	kind := cfg.GroupKinds["tag"]
	kind.Windows = true
	cfg.GroupKinds["tag"] = kind
	var err error
	f.groups, err = groups.New(groups.Options{Pool: f.pool, Schema: f.schema, Tenant: tenant, Kinds: cfg})
	if err != nil {
		t.Fatal(err)
	}
	static, dated, parent := f.group("tag", "static"), f.group("tag", "dated"), f.group("tag", "parent")
	parentID := groups.GroupID(parent)
	if _, err := f.groups.UpdateGroups(f.ctx, []groups.GroupUpdate{{ID: groups.GroupID(static), ParentID: &parentID}}); err != nil {
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
	if _, err := f.pool.Exec(f.ctx, `UPDATE `+f.table("content_memberships")+` SET active_from=now()-interval '3 days' WHERE group_id=$1`, dated); err != nil {
		t.Fatal(err)
	}
	check("Static", "Parent")
	if _, err := f.pool.Exec(f.ctx, `UPDATE `+f.table("content_memberships")+` SET active_until=now()-interval '2 days' WHERE group_id=$1`, dated); err != nil {
		t.Fatal(err)
	}
	check("Static", "Parent")
}
