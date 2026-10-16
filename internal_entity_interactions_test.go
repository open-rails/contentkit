package contentkit

import (
	"context"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
)

func TestInternalEntitiesCannotReceivePublicInteractions(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	e, err := New(t.Context(), siteConfig(engineSchema(t, pool)), Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	created, err := e.groups.CreateGroups(t.Context(), []groups.GroupInput{
		{Kind: "tag", Slug: "public"}, {Kind: "tag", Slug: "internal", Internal: true}, {Kind: "tag", Slug: "merged", Internal: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.groups.Merge(t.Context(), []groups.Merge{{From: created[2].ID, Into: created[1].ID}}); err != nil {
		t.Fatal(err)
	}
	refs := []contentref.ContentRef{}
	for _, entity := range created {
		refs = append(refs, contentref.New(e.cfg.Tenant, entity.Kind, string(entity.ID)))
	}
	for _, actor := range []access.Actor{{Anonymous: true}, {ID: "reader"}, {ID: "staff"}} {
		got, err := (interactionTargets{e}).Resolve(t.Context(), refs, actor)
		if err != nil {
			t.Fatal(err)
		}
		if !got[refs[0].Key()].Visible {
			t.Fatal("public entity was hidden")
		}
		for _, ref := range refs[1:] {
			if got[ref.Key()].Visible {
				t.Fatalf("internal entity accepted interaction from %+v: %s", actor, ref)
			}
		}
	}
}
