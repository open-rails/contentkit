package entities

import (
	"errors"
	"testing"
)

// Site-owned kinds that declare an avatar take one; other site kinds refuse it.
func TestSiteOwnedAvatarKindsIntegration(t *testing.T) {
	f := newFixture(t)
	made := f.create(EntityInput{Kind: "voice_actor", Slug: "seiyuu", Names: []Name{canonical("en", "Seiyuu")}},
		EntityInput{Kind: "tag", Slug: "plain", Names: []Name{canonical("en", "Plain")}})
	image := "i-01920000-0000-7000-8000-000000000001"
	got, err := f.s.UpdateEntities(f.ctx, []EntityUpdate{{ID: made[0].ID, Avatar: &Avatar{Image: image}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Avatar == nil || got[0].Avatar.Image != image {
		t.Fatalf("voice actor avatar: %+v", got)
	}
	if _, err := f.s.UpdateEntities(f.ctx, []EntityUpdate{{ID: made[1].ID, Avatar: &Avatar{Image: image}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tag avatar: %v, want ErrInvalid", err)
	}
}

// Management lists the restorable deleted entities on their own; public reads cannot.
func TestDeletedEntitiesListIntegration(t *testing.T) {
	f := newFixture(t)
	made := f.create(EntityInput{Kind: "tag", Slug: "kept", Names: []Name{canonical("en", "Kept")}},
		EntityInput{Kind: "tag", Slug: "gone", Names: []Name{canonical("en", "Gone")}})
	gone := StateDeleted
	if _, err := f.s.UpdateEntities(f.ctx, []EntityUpdate{{ID: made[1].ID, State: &gone}}); err != nil {
		t.Fatal(err)
	}
	slugs := func(opts ListOptions) []string {
		t.Helper()
		page, err := f.s.List(f.ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, g := range page.Entities {
			out = append(out, g.Slug)
		}
		return out
	}
	if got := slugs(ListOptions{Kind: "tag", IncludeInternal: true}); len(got) != 1 || got[0] != "kept" {
		t.Fatalf("active list %v", got)
	}
	if got := slugs(ListOptions{Kind: "tag", IncludeInternal: true, Deleted: true}); len(got) != 1 || got[0] != "gone" {
		t.Fatalf("deleted list %v", got)
	}
	if _, err := f.s.List(f.ctx, ListOptions{Kind: "tag", Deleted: true}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("public deleted list: %v, want ErrForbidden", err)
	}
}
