package critical_test

import (
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/kinds"
)

func TestImportMergedEntityMemberships(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["collection"] = kinds.EntityKind{Route: "collections"}
		article := cfg.Content.Kinds["article"]
		article.Entities = []string{"collection"}
		cfg.Content.Kinds["article"] = article
	})
	a := w.apps[0]
	admin, ctx := a.engine.Client(), t.Context()
	batch := importer.Batch{Source: "merged-memberships", Entities: []importer.EntityRecord{
		{ExternalID: "a", Kind: "collection", Slug: "legacy-a", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Legacy A"}}},
		{ExternalID: "b", Kind: "collection", Slug: "survivor-b", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Survivor B"}}},
	}, Contents: []importer.ContentRecord{{ExternalID: "content", Kind: "article", State: "draft"}},
		Memberships: []importer.MembershipSet{{Content: importer.Ref{ExternalID: "content"}, Kinds: []string{"collection"},
			Memberships: []importer.MembershipRecord{{Entity: importer.Ref{ExternalID: "a"}}}}},
	}
	initial, err := admin.PostImport(ctx, batch)
	must(t, err)
	if len(initial.Failed) != 0 || len(initial.Skipped) != 0 || len(initial.Merged) != 0 || initial.Written[importer.SectionEntities] != 2 || initial.Written[importer.SectionContents] != 1 || initial.Written[importer.SectionMemberships] != 1 {
		t.Fatalf("initial import: %+v", initial)
	}
	from, into := initial.IDs[importer.RecordEntity]["a"], initial.IDs[importer.RecordEntity]["b"]
	contentID := initial.IDs[importer.RecordContent]["content"]
	_, err = admin.PostEntityMerge(ctx, from, entities.MergeInput{Into: entities.EntityID(into)})
	must(t, err)
	before, err := admin.GetEntity(ctx, into, ck.GetEntityQuery{})
	must(t, err)
	tombstoneSQL := `SELECT to_jsonb(e)::text FROM ` + pgx.Identifier{w.schema, "content_entities"}.Sanitize() +
		` e WHERE tenant_id=$1 AND id=$2`
	var tombstoneBefore string
	must(t, w.pool.QueryRow(ctx, tombstoneSQL, a.cfg.Scope.AppID.String(), from).Scan(&tombstoneBefore))
	batch.Entities = batch.Entities[:1]
	batch.Contents = nil
	// Direct and external references to either identity collapse onto B.
	batch.Memberships[0].Memberships = append(batch.Memberships[0].Memberships,
		importer.MembershipRecord{Entity: importer.Ref{ID: from}},
		importer.MembershipRecord{Entity: importer.Ref{ExternalID: "b"}})
	result, err := admin.PostImport(ctx, batch)
	must(t, err)
	if len(result.Failed) != 0 || len(result.Skipped) != 0 || !reflect.DeepEqual(result.Merged, map[string]int{importer.SectionEntities: 1}) || result.Written[importer.SectionEntities] != 0 || result.Written[importer.SectionMemberships] != 1 {
		t.Fatalf("replayed import: %+v", result)
	}
	if len(result.IDs[importer.RecordEntity]) != 0 {
		t.Fatalf("unwritten merged entity reported an id: %+v", result.IDs)
	}
	memberships, err := admin.GetContentEntities(ctx, contentID, ck.GetContentEntitiesQuery{})
	must(t, err)
	if len(memberships.Memberships) != 1 || memberships.Memberships[0].EntityID != entities.EntityID(into) {
		t.Fatalf("replayed memberships: %+v", memberships)
	}
	after, err := admin.GetEntity(ctx, into, ck.GetEntityQuery{})
	must(t, err)
	if after.Slug != before.Slug || !reflect.DeepEqual(after.Names, before.Names) || after.Revision != before.Revision {
		t.Fatalf("replay changed survivor: before %+v; after %+v", before, after)
	}
	var state, mergedInto string
	must(t, w.pool.QueryRow(ctx, `SELECT state, merged_into FROM `+pgx.Identifier{w.schema, "content_entities"}.Sanitize()+
		` WHERE tenant_id=$1 AND id=$2`, a.cfg.Scope.AppID.String(), from).Scan(&state, &mergedInto))
	if state != string(entities.StateMerged) || mergedInto != into {
		t.Fatalf("replay changed merged entity: state=%s merged_into=%s", state, mergedInto)
	}
	var tombstoneAfter string
	must(t, w.pool.QueryRow(ctx, tombstoneSQL, a.cfg.Scope.AppID.String(), from).Scan(&tombstoneAfter))
	if tombstoneAfter != tombstoneBefore {
		t.Fatalf("replay changed tombstone: before %s; after %s", tombstoneBefore, tombstoneAfter)
	}
}

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
