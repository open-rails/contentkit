package critical_test

import (
	"reflect"
	"testing"

	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
)

// Ordinary associations remain immediate, ordered and scoped after removing
// scheduled collection membership. No background engine is started here.
func TestEntityMembershipsWithoutTimers(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["collection"] = kinds.EntityKind{Route: "collections", Ordered: true, Parent: "collection"}
		article := cfg.Content.Kinds["article"]
		article.Entities = []string{"collection"}
		cfg.Content.Kinds["article"] = article
	})
	a := w.apps[0]
	persona, err := w.auth.Persona("app")
	must(t, err)
	key, err := w.tenant[0].CreateAPIKey(t.Context(), iam.SystemIdentity(), a.group, iam.NewAPIKey{Name: "Collection editor", Role: persona.OwnerRole()})
	must(t, err)
	staff, public := a.client(key.Secret), a.client("")
	create := func(slug string, parent entities.EntityID) entities.Entity {
		rows, err := staff.PostEntities(t.Context(), []entities.EntityInput{{Kind: "collection", Slug: slug, ParentID: parent,
			Names: []entities.Name{{Language: "en", Kind: "name", Name: slug}}}})
		must(t, err)
		if len(rows) != 1 {
			t.Fatalf("created %d entities; want one", len(rows))
		}
		return rows[0]
	}
	parent := create("parent-collection", "")
	child := create("child-collection", parent.ID)
	one := publishedArticle(t, a, "First collection member")
	two := publishedArticle(t, a, "Second collection member")
	proposed := publishedArticle(t, a, "Proposed collection member")
	_, err = staff.PostEntityMembers(t.Context(), string(child.ID), []entities.Membership{
		{ContentID: one.ID}, {ContentID: two.ID}, {ContentID: proposed.ID, State: entities.MembershipProposed},
	})
	must(t, err)
	_, err = staff.PutEntityMembersOrder(t.Context(), string(child.ID), entities.Order{Members: []entities.MemberRef{{ContentID: two.ID}, {ContentID: one.ID}}})
	must(t, err)
	check := func() {
		page, err := public.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Entities: []string{string(child.ID)}, Sort: "position"})
		must(t, err)
		var ids []string
		for _, card := range page.Contents {
			ids = append(ids, card.ID)
		}
		if !reflect.DeepEqual(ids, []string{two.ID, one.ID}) {
			t.Fatalf("ordered active members = %v; want [%s %s]", ids, two.ID, one.ID)
		}
		ancestors, err := public.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Entities: []string{string(parent.ID)}})
		must(t, err)
		assertCards(t, ancestors, one.ID, two.ID)
	}
	check()
}
