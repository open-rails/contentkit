package catalog

import (
	"slices"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

// The one visibility rule: site scopes (excluded restrictions, required
// groups), content filters (config defaults and a viewer's overrides), the
// listing rule (listed, members, unlisted), lock and hide modes, and the
// viewers who see everything (staff, creators, owner-group editors), for
// lists (Listed) and direct reads (Contents) alike.
func TestVisibilityIntegration(t *testing.T) {
	f := newFixture(t, nil)
	yaoi, gore, red := f.group("tag", "yaoi"), f.group("tag", "gore"), f.group("tag", "red")
	studio := f.group("channel", "studio")
	f.editor("studio", "ed")

	item := func(title string, edit func(*ContentInput), languages ...string) string {
		in := gallery(title, languages...)
		if edit != nil {
			edit(&in)
		}
		id := f.create("staff", in)
		f.publish("staff", id, nil)
		return id
	}
	plain := item("Plain", nil, "en", "ja")
	restricted := item("Restricted", func(c *ContentInput) { c.Restrictions = []string{"restricted"} }, "en")
	gay := item("Gay", nil, "en")
	f.join(yaoi, gay, "")
	gory := item("Gory", nil, "en")
	f.join(gore, gory, "")
	reddish := item("Red", nil, "ja")
	f.join(red, reddish, "")
	members := item("Members", func(c *ContentInput) {
		c.Listing, c.Access = ListingMembers, &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}
	}, "en")
	unlisted := item("Unlisted", func(c *ContentInput) { c.Listing = ListingUnlisted }, "en")
	premium := item("Premium", func(c *ContentInput) { c.Access = &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}} }, "en")
	channel := item("Channel", func(c *ContentInput) { c.Access = &access.Rule{Level: access.GroupLevel, Group: studio} }, "en")
	ppv := item("Pay per view", func(c *ContentInput) { c.Access = &access.Rule{Level: access.PPV} }, "en")
	draft := f.create("staff", func() ContentInput {
		in := gallery("Draft", "en")
		in.OwnerGroupID, in.Creators = studio, &[]Creator{{UserID: "alice"}}
		return in
	}())
	f.claim("pam", "premium")
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO `+f.table("content_filter_prefs")+` (tenant_id, user_id, overrides) VALUES ($1, 'carol', $2)`,
		tenant, `{"`+gore+`": "show", "`+red+`": "hide"}`); err != nil {
		t.Fatal(err)
	}

	everyLive := []string{plain, restricted, gay, gory, reddish, members, unlisted, premium, channel, ppv}
	for _, c := range []struct {
		name, actor, host string
		o                 ViewerOptions
		want              []string
	}{
		{"anonymous on main: listed, outside the default filter", anon, "main.test", ViewerOptions{},
			[]string{plain, restricted, gay, reddish, premium, channel, ppv}},
		{"unmatched host is the first site", anon, "elsewhere.test", ViewerOptions{},
			[]string{plain, restricted, gay, reddish, premium, channel, ppv}},
		{"the restricted site leaves restricted content out", anon, "moe.test", ViewerOptions{},
			[]string{plain, gay, reddish, premium, channel, ppv}},
		{"the gay site shows only its group", anon, "gay.test", ViewerOptions{}, []string{gay}},
		{"a tier holder also lists the members listing it opens", "pam", "main.test", ViewerOptions{},
			[]string{plain, restricted, gay, reddish, members, premium, channel, ppv}},
		{"hide mode: only what the viewer opens", anon, "main.test", ViewerOptions{Mode: access.Hide}, []string{plain, restricted, gay, reddish}},
		{"hide mode for a tier holder", "pam", "main.test", ViewerOptions{Mode: access.Hide}, []string{plain, restricted, gay, reddish, members, premium}},
		{"a viewer's overrides: show gore, hide red", "carol", "main.test", ViewerOptions{},
			[]string{plain, restricted, gay, gory, premium, channel, ppv}},
		{"a language lists only items with a live version serving it", anon, "main.test", ViewerOptions{Language: "ja"}, []string{plain, reddish}},
		{"staff see everything, every site", "staff", "moe.test", ViewerOptions{}, append(everyLive, draft)},
		{"view-restricted sees everything", "viewer", "gay.test", ViewerOptions{}, append(everyLive, draft)},
		{"a creator also lists their draft", "alice", "main.test", ViewerOptions{},
			[]string{plain, restricted, gay, reddish, premium, channel, ppv, draft}},
		{"an owner group's editor lists its drafts", "ed", "main.test", ViewerOptions{EditorGroups: []string{studio}},
			[]string{plain, restricted, gay, reddish, premium, channel, ppv, draft}},
		{"another channel's editor does not", "bob", "main.test", ViewerOptions{EditorGroups: []string{studio}},
			[]string{plain, restricted, gay, reddish, premium, channel, ppv}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, want := f.listed(f.viewer(c.actor, c.host, c.o)), sorted(c.want...); !slices.Equal(got, want) {
				t.Fatalf("listed %v\n want %v", got, want)
			}
		})
	}

	// Direct reads ignore the listing rule and the filters, keep the site
	// scope, and decide each version's access.
	got := f.read(anon, "main.test", unlisted, gory, members, premium, channel, ppv, draft)
	if got[0] == nil || got[1] == nil || got[2] == nil || got[6] != nil {
		t.Fatalf("anonymous direct reads %v", ids(got))
	}
	// Sell: the rule's own keys (tiers, group), then the version's and the content's.
	vkey := func(c *Content) string { return tenant + ":content:" + c.ID + "/" + c.Versions[0].ID }
	ckey := func(c *Content) string { return tenant + ":content:" + c.ID }
	if d := got[3].Versions[0].Decision; d.Allowed || !slices.Equal(d.Sell, []string{"premium", vkey(got[3]), ckey(got[3])}) {
		t.Fatalf("anonymous on premium %+v", d)
	}
	if d := got[4].Versions[0].Decision; d.Allowed || !slices.Equal(d.Sell, []string{tenant + ":group:" + studio, vkey(got[4]), ckey(got[4])}) {
		t.Fatalf("anonymous on a channel's item %+v", d)
	}
	if d := got[5].Versions[0].Decision; d.Allowed || !slices.Equal(d.Sell, []string{vkey(got[5]), ckey(got[5])}) {
		t.Fatalf("anonymous on pay per view %+v", d)
	}
	got = f.read("pam", "main.test", channel, ppv, premium)
	if got[0].Versions[0].Decision.Allowed || got[1].Versions[0].Decision.Allowed || !got[2].Versions[0].Decision.Allowed {
		t.Fatalf("the tier holder's decisions %+v %+v %+v", got[0].Versions[0].Decision, got[1].Versions[0].Decision, got[2].Versions[0].Decision)
	}
	if got := f.read(anon, "moe.test", restricted)[0]; got != nil {
		t.Fatal("a restricted item reads on the restricted site")
	}
	if got := f.read("staff", "moe.test", restricted)[0]; got == nil || !got.Editor {
		t.Fatal("staff do not read the restricted item on the restricted site")
	}
	if got := f.read("ed", "main.test", draft)[0]; got == nil || !got.Editor || !got.Versions[0].Decision.Allowed {
		t.Fatalf("the owner group's editor reads the draft: %+v", got)
	}
	if got := f.read("ed", "main.test", plain)[0]; got == nil || got.Editor {
		t.Fatalf("an editor of another group edits plain content: %+v", got)
	}
	if err := f.s.Update(f.ctx, f.as("ed"), []Update{{ID: draft, ContentPatch: ContentPatch{Listing: ptr(ListingUnlisted)}}}); err != nil {
		t.Fatalf("the owner group's editor edits: %v", err)
	}
	if err := f.s.Update(f.ctx, f.as("ed"), []Update{{ID: plain, ContentPatch: ContentPatch{Listing: ptr(ListingUnlisted)}}}); err == nil {
		t.Fatal("an editor edited content outside its group")
	}

	// Groups summarize what an item belongs to.
	if g := f.read(anon, "main.test", gay)[0].Groups; len(g) != 1 || g[0].ID != yaoi || g[0].Name != "Yaoi" || g[0].Kind != "tag" {
		t.Fatalf("groups %+v", g)
	}
}

// The resolver answers content and media gating for a content reference
// and each version, by the same rule.
func TestResolverIntegration(t *testing.T) {
	f := newFixture(t, nil)
	in := gallery("Resolved", "en", "ja")
	in.Access = &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}
	in.Creators = &[]Creator{{UserID: "alice"}}
	id := f.create("staff", in)
	f.publish("staff", id, nil)
	vs := f.read("staff", "main.test", id)[0].Versions
	draft := f.create("staff", gallery("Draft", "en"))
	f.claim("pam", "premium")
	ref := contentref.New(tenant, "gallery", id)
	refs := []contentref.ContentRef{ref, ref.WithVersion(vs[1].ID), ref.WithVersion("missing"), contentref.New(tenant, "gallery", draft),
		contentref.New(tenant, "blog", id), contentref.New("other", "gallery", id)}
	for _, c := range []struct {
		actor string
		want  map[contentref.ContentKey]access.Resolution
	}{
		{anon, map[contentref.ContentKey]access.Resolution{ref.Key(): {Ref: ref, Visible: true, Owner: "alice"},
			ref.WithVersion(vs[1].ID).Key(): {Ref: ref.WithVersion(vs[1].ID), Visible: true, Owner: "alice"}}},
		{"pam", map[contentref.ContentKey]access.Resolution{ref.Key(): {Ref: ref, Visible: true, Accessible: true, Owner: "alice"},
			ref.WithVersion(vs[1].ID).Key(): {Ref: ref.WithVersion(vs[1].ID), Visible: true, Accessible: true, Owner: "alice"}}},
		{"staff", map[contentref.ContentKey]access.Resolution{ref.Key(): {Ref: ref, Visible: true, Accessible: true, Editor: true, Owner: "alice"},
			ref.WithVersion(vs[1].ID).Key():                {Ref: ref.WithVersion(vs[1].ID), Visible: true, Accessible: true, Editor: true, Owner: "alice"},
			contentref.New(tenant, "gallery", draft).Key(): {Ref: contentref.New(tenant, "gallery", draft), Visible: true, Accessible: true, Editor: true, Owner: "staff"}}},
	} {
		got, err := f.s.Resolver().Resolve(access.WithMemo(f.ctx), refs, f.as(c.actor))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(c.want) {
			t.Errorf("%q: %v", c.actor, got)
		}
		for k, want := range c.want {
			g := got[k]
			if g.Ref.Key() != want.Ref.Key() || g.Visible != want.Visible || g.Accessible != want.Accessible || g.Editor != want.Editor || g.Owner != want.Owner {
				t.Errorf("%q %v: %+v, want %+v", c.actor, k, got[k], want)
			}
		}
	}

	// A locked version's decision carries the rule's preview and free_after.
	free := f.now().Add(time.Hour)
	preview := &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}, Preview: &access.Preview{Segments: 1}, FreeAfter: &free}
	if err := f.s.Update(f.ctx, f.as("staff"), []Update{{ID: id, ContentPatch: ContentPatch{Access: preview}}}); err != nil {
		t.Fatal(err)
	}
	if d := f.read(anon, "main.test", id)[0].Versions[0].Decision; d.Allowed || d.Preview == nil || d.Preview.Segments != 1 || d.FreeAfter == nil || !d.FreeAfter.Equal(free) {
		t.Fatalf("the locked decision %+v", d)
	}
}

// Segments release in order: one is open once it and every earlier one
// passed their time.
func TestSegmentsReleaseInOrderIntegration(t *testing.T) {
	f := newFixture(t, nil)
	id := f.create("staff", gallery("Chapters", "en"))
	f.publish("staff", id, nil)
	v := f.read("staff", "main.test", id)[0].Versions[0]
	soon, later := f.now().Add(time.Hour), f.now().Add(2*time.Hour)
	segs := []Segment{{Title: "One", Start: 0, End: 10}, {Title: "Two", Start: 10, End: 20, ReleaseAt: &later}, {Title: "Three", Start: 20, End: 30, ReleaseAt: &soon}}
	if _, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionID: v.ID, VersionInput: VersionInput{Segments: &segs}}}); err != nil {
		t.Fatal(err)
	}
	released := func() []bool {
		var out []bool
		for _, s := range f.read(anon, "main.test", id)[0].Versions[0].Segments {
			out = append(out, s.Released)
		}
		return out
	}
	if got := released(); !slices.Equal(got, []bool{true, false, false}) {
		t.Fatalf("now %v", got)
	}
	f.advance(90 * time.Minute)
	if got := released(); !slices.Equal(got, []bool{true, false, false}) {
		t.Fatalf("after the third's time, before the second's %v", got)
	}
	f.advance(time.Hour)
	if got := released(); !slices.Equal(got, []bool{true, true, true}) {
		t.Fatalf("after both %v", got)
	}
}
