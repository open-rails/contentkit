package taxonomy

import (
	"context"
	"github.com/open-rails/contentkit/contentref"
	"testing"
)

func TestAddingEquivalentAliasKeepsCanonicalDocument(t *testing.T) {
	ctx := context.Background()
	pool, schema := testSchema(t, ctx)
	s := newStore(t, pool, schema, tenant, nil)
	mustCreate(t, ctx, s, tag("tag-a", "tag-a", name("en", "Cafe")))
	if err := s.AddNames(ctx, "tag-a", []Name{alias("en", "CAFE")}); err != nil {
		t.Fatal(err)
	}
	docs, err := s.BuildKeywordDocuments(ctx, tenant, "tag", "en", []contentref.ContentRef{contentref.New(tenant, "tag", "tag-a")})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Title != "Cafe" {
		t.Fatalf("equivalent alias removed canonical document: %+v", docs)
	}
}

func TestMergePreservesActiveAssignmentOverProposedDuplicate(t *testing.T) {
	ctx := context.Background()
	pool, schema := testSchema(t, ctx)
	s := newStore(t, pool, schema, tenant, nil)
	mustCreate(t, ctx, s, tag("source", "source", name("en", "Source")), tag("target", "target", name("en", "Target")))
	ref := contentref.NewVersion(tenant, "gallery", "work", "version")
	source, target := assign(ref, "source", "tag"), assign(ref, "target", "tag")
	target.State = AssignmentProposed
	if err := s.Assign(ctx, []Assignment{source, target}, AssignOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Merge(ctx, "source", "target"); err != nil {
		t.Fatal(err)
	}
	effective, err := s.EffectiveTags(ctx, []contentref.ContentRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(effective[ref.Key()]) != 1 {
		t.Fatalf("merge lost active assignment behind proposed target: %+v", effective)
	}
}
