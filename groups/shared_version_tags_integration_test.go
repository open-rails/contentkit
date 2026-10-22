package groups

import (
	"slices"
	"testing"

	"github.com/open-rails/contentkit/kinds"
)

func TestTagEntitiesInheritSharedScopeWithoutLeakingSiblingVersions(t *testing.T) {
	f := newFixture(t)
	tag := f.s.cfg.GroupKinds["tag"]
	tag.VersionLevel = false
	tag.MembershipScopes = []kinds.MembershipScope{kinds.ContentScope, kinds.VersionScope}
	tag.Creatable = true
	f.s.cfg.GroupKinds["tag"] = tag
	f.content("gallery", 1, live("en"), live("es"))
	tags := f.create(GroupInput{Kind: "tag", Slug: "shared"}, GroupInput{Kind: "tag", Slug: "werewolf"}, GroupInput{Kind: "tag", Slug: "vampire"})
	set := func(scope string, ids ...GroupID) {
		t.Helper()
		ms := []Membership{}
		for _, id := range ids {
			ms = append(ms, Membership{GroupID: id})
		}
		if err := f.s.SetContentGroups(f.ctx, []ContentGroups{{ContentID: cid(1), VersionScope: &scope, Kinds: []string{"tag"}, Memberships: ms}}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(first, second []string) {
		t.Helper()
		got, err := f.s.EffectiveGroups(f.ctx, f.pool, []string{vid(1, 0), vid(1, 1)})
		if err != nil {
			t.Fatal(err)
		}
		for id, want := range map[string][]string{vid(1, 0): first, vid(1, 1): second} {
			slices.Sort(want)
			if !slices.Equal(got[id], want) {
				t.Fatalf("%s effective tags=%v, want %v", id, got[id], want)
			}
		}
	}
	shared, werewolf, vampire := string(tags[0].ID), string(tags[1].ID), string(tags[2].ID)
	set("", tags[0].ID)
	set(vid(1, 0), tags[0].ID, tags[1].ID)
	set(vid(1, 1), tags[2].ID)
	check([]string{shared, werewolf}, []string{shared, vampire})
	set(vid(1, 0))
	check([]string{shared}, []string{shared, vampire})
	version := vid(1, 0)
	if err := f.s.SetContentGroups(f.ctx, []ContentGroups{{ContentID: cid(1), VersionScope: &version, Kinds: []string{"tag"}, Create: []NewEntity{{Kind: "tag", Name: "Tiger", Language: "en"}}}}); err != nil {
		t.Fatal(err)
	}
	all, err := f.s.MembershipsOf(f.ctx, []string{cid(1)})
	if err != nil {
		t.Fatal(err)
	}
	var created string
	for _, m := range all[cid(1)] {
		if m.VersionID == version {
			created = string(m.GroupID)
		}
	}
	if created == "" || created == shared || created == werewolf || created == vampire {
		t.Fatal("typed tag was not created in the selected version")
	}
	check([]string{shared, created}, []string{shared, vampire})
}
