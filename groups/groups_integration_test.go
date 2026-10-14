package groups

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/contenturl"
)

// Groups are created against their kind's rules: scope, parent, owner, auth
// scope, declared attributes and cover; slugs are unique per kind and scope.
func TestCreateGroupsFollowsKinds(t *testing.T) {
	f := newFixture(t)
	made := f.create(
		GroupInput{Kind: "series", Slug: "fate", Names: []Name{canonical("en", "Fate"), canonical("ja", "フェイト")}, Text: []Text{{Language: "en", Description: "A saga", SortKey: "fate"}}},
		GroupInput{Kind: "series", Slug: "fate-zero", Names: []Name{canonical("en", "Fate/Zero")}},
		GroupInput{Kind: "artist", Slug: "shindol", Names: []Name{canonical("en", "Shindol"), alias("en", "Shin")}, Attributes: map[string]any{"artist_type": "circle"}},
	)
	fate, zero, shindol := made[0], made[1], made[2]
	if fate.State != StateActive || fate.Kind != "series" || len(shindol.Attributes) != 1 || shindol.Attributes["artist_type"] != "circle" {
		t.Fatalf("created %+v %+v", fate, shindol)
	}
	chars := f.create(
		GroupInput{Kind: "character", Slug: "saber", ScopeID: fate.ID, Names: []Name{canonical("en", "Saber")}},
		GroupInput{Kind: "character", Slug: "saber", ScopeID: zero.ID, Names: []Name{canonical("en", "Saber")}, Attributes: map[string]any{"external_link": "https://x.test"}},
	)
	got, err := f.s.UpdateGroups(f.ctx, []GroupUpdate{{ID: zero.ID, ParentID: &fate.ID}})
	if err != nil || got[0].ParentID != fate.ID || got[0].Revision != 2 {
		t.Fatalf("parent: %+v %v", got, err)
	}

	for name, in := range map[string]GroupInput{
		"unknown kind":          {Kind: "studio", Slug: "x"},
		"slug with a slash":     {Kind: "tag", Slug: "a/b"},
		"scoped without scope":  {Kind: "character", Slug: "rin"},
		"scope on a tag":        {Kind: "tag", Slug: "x", ScopeID: fate.ID},
		"parent on a tag":       {Kind: "tag", Slug: "x", ParentID: fate.ID},
		"site group with owner": {Kind: "tag", Slug: "x", OwnerUserID: "u1"},
		"user group no owner":   {Kind: "channel", Slug: "x"},
		"auth scope on a tag":   {Kind: "tag", Slug: "x", AuthScope: "g1"},
		"undeclared attribute":  {Kind: "tag", Slug: "x", Attributes: map[string]any{"color": "red"}},
		"enum value":            {Kind: "artist", Slug: "x", Attributes: map[string]any{"artist_type": "band"}},
		"cover image name":      {Kind: "tag", Slug: "x", Cover: &Cover{Image: "cover.png"}},
		"cover of both":         {Kind: "tag", Slug: "x", Cover: &Cover{Image: "i-0193a000-0000-7000-8000-000000000001", ContentID: cid(1)}},
		"two canonical names":   {Kind: "tag", Slug: "x", Names: []Name{canonical("en", "A"), canonical("en", "B")}},
		"scope of wrong kind":   {Kind: "character", Slug: "x", ScopeID: shindol.ID},
		"parent of wrong kind":  {Kind: "series", Slug: "x", ParentID: shindol.ID},
	} {
		if _, err := f.s.CreateGroups(f.ctx, []GroupInput{in}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, in := range map[string]GroupInput{
		"slug taken":              {Kind: "series", Slug: "fate"},
		"slug taken in its scope": {Kind: "character", Slug: "saber", ScopeID: fate.ID},
		"id taken":                {ID: fate.ID, Kind: "tag", Slug: "new"},
	} {
		if _, err := f.s.CreateGroups(f.ctx, []GroupInput{in}); !errors.Is(err, ErrConflict) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.s.CreateGroups(f.ctx, []GroupInput{{Kind: "tag", Slug: "x", Cover: &Cover{ContentID: cid(9)}}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("cover of missing content: %v", err)
	}
	if _, err := f.s.UpdateGroups(f.ctx, []GroupUpdate{{ID: fate.ID, ParentID: &zero.ID}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("parent cycle: %v", err)
	}
	if _, err := f.s.CreateGroups(f.ctx, []GroupInput{{Kind: "channel", Slug: "mine", OwnerUserID: "u1", AuthScope: "ak-group-1"}}); err != nil {
		t.Errorf("channel: %v", err)
	}

	// Codes and slugs come from the canonical names; documents are queued per language.
	urls, err := contenturl.New(contenturl.Options{Pool: f.pool, Schema: f.schema, Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	ref := contentref.New(tenant, "series", string(fate.ID))
	links, err := urls.Links(f.ctx, []contentref.ContentRef{ref, contentref.New(tenant, "character", string(chars[0].ID))})
	if err != nil || len(links) != 2 || links[ref.Key()].Slug != "fate" {
		t.Fatalf("links %+v %v", links, err)
	}
	var dirty int
	if err := f.pool.QueryRow(f.ctx, fmt.Sprintf(`SELECT count(*) FROM %q.content_search_dirty WHERE content_kind = 'series' AND content_id = $1`, f.schema), string(fate.ID)).Scan(&dirty); err != nil || dirty != 2 {
		t.Fatalf("%d dirty documents, want one per language: %v", dirty, err)
	}

	// Deleting frees the slug; restoring the old group then conflicts.
	if _, err := f.s.DeleteGroups(f.ctx, []GroupID{shindol.ID}); err != nil {
		t.Fatal(err)
	}
	f.create(GroupInput{Kind: "artist", Slug: "shindol"})
	if _, err := f.s.UpdateGroups(f.ctx, []GroupUpdate{{ID: shindol.ID, State: ptr(StateActive)}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("restore over a taken slug: %v", err)
	}
	if _, err := f.s.UpdateGroups(f.ctx, []GroupUpdate{{ID: shindol.ID, State: ptr(StateMerged)}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("state merged is not assignable: %v", err)
	}
}

// Names: a new canonical name demotes the old one to an alias; set, add and
// remove are batches.
func TestNames(t *testing.T) {
	f := newFixture(t)
	a := f.create(GroupInput{Kind: "tag", Slug: "a", Names: []Name{canonical("en", "Colored")}})[0]
	if err := f.s.AddNames(f.ctx, []GroupNames{{ID: a.ID, Names: []Name{canonical("en", "Full Color"), alias("es", "Coloreado")}}}); err != nil {
		t.Fatal(err)
	}
	d, err := f.s.Detail(f.ctx, a.ID, "es")
	if err != nil {
		t.Fatal(err)
	}
	want := []Name{canonical("en", "Full Color"), alias("en", "Colored"), alias("es", "Coloreado")}
	if !slices.Equal(d.Names, want) || d.Name != "Full Color" || d.NameLanguage != "en" {
		t.Fatalf("names %+v (%s/%s)", d.Names, d.Name, d.NameLanguage)
	}
	if err := f.s.RemoveNames(f.ctx, []GroupNames{{ID: a.ID, Names: []Name{{Language: "en", Name: "colored"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetNames(f.ctx, []GroupNames{{ID: a.ID, Names: []Name{canonical("es", "Color")}}}); err != nil {
		t.Fatal(err)
	}
	if d, _ = f.s.Detail(f.ctx, a.ID, "en"); len(d.Names) != 1 || d.Name != "Color" || d.Slug != "a" {
		t.Fatalf("after set %+v", d)
	}
	if err := f.s.AddNames(f.ctx, []GroupNames{{ID: GroupID(cid(99)), Names: []Name{canonical("en", "x")}}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("names of a missing group: %v", err)
	}
}

// A version's effective groups: its content's and its own active memberships
// in active groups, inside their windows, with every active ancestor.
func TestEffectiveGroups(t *testing.T) {
	f := newFixture(t)
	f.content("gallery", 1, live("en"), live("es"))
	f.content("gallery", 2, live("en"))
	g := f.create(
		GroupInput{Kind: "series", Slug: "root"},
		GroupInput{Kind: "tag", Slug: "tag"},
		GroupInput{Kind: "voice_actor", Slug: "va"},
		GroupInput{Kind: "rail", Slug: "now"},
		GroupInput{Kind: "rail", Slug: "later"},
		GroupInput{Kind: "rail", Slug: "over"},
		GroupInput{Kind: "tag", Slug: "proposed"},
		GroupInput{Kind: "tag", Slug: "gone"},
	)
	root, tag, va, now, later, over, proposed, gone := g[0], g[1], g[2], g[3], g[4], g[5], g[6], g[7]
	sub := f.create(GroupInput{Kind: "series", Slug: "sub", ParentID: root.ID})[0]
	hour := time.Now().Add(time.Hour)
	ago := time.Now().Add(-time.Minute)
	f.changed = nil
	f.add(
		Membership{GroupID: sub.ID, ContentID: cid(1)},
		Membership{GroupID: tag.ID, ContentID: cid(1)},
		Membership{GroupID: va.ID, ContentID: cid(1), VersionID: vid(1, 1)},
		Membership{GroupID: now.ID, ContentID: cid(1), ActiveUntil: &hour},
		Membership{GroupID: later.ID, ContentID: cid(1), ActiveFrom: &hour},
		Membership{GroupID: over.ID, ContentID: cid(1), ActiveUntil: &ago},
		Membership{GroupID: proposed.ID, ContentID: cid(1), State: MembershipProposed},
		Membership{GroupID: gone.ID, ContentID: cid(2)},
	)
	if len(f.changed) != 1 || !slices.Equal(f.changed[0], []string{cid(1), cid(2)}) {
		t.Fatalf("ContentChanged calls %v", f.changed)
	}
	eff, err := f.s.EffectiveGroups(f.ctx, f.pool, []string{vid(1, 0), vid(1, 1), vid(2, 0), "missing"})
	if err != nil {
		t.Fatal(err)
	}
	v0 := ids(root, sub, tag, now)
	v1 := ids(root, sub, tag, va, now)
	if !slices.Equal(eff[vid(1, 0)], v0) || !slices.Equal(eff[vid(1, 1)], v1) || !slices.Equal(eff[vid(2, 0)], ids(gone)) || len(eff) != 3 {
		t.Fatalf("effective %v", eff)
	}
	if facets := f.facetGroups(); !slices.Equal(facets[vid(1, 0)], v0) || !slices.Equal(facets[vid(1, 1)], v1) {
		t.Fatalf("facets %v", facets)
	}

	// Deleting a group, or its ancestor, takes it out of every facet.
	if _, err := f.s.DeleteGroups(f.ctx, []GroupID{gone.ID, root.ID}); err != nil {
		t.Fatal(err)
	}
	if facets := f.facetGroups(); !slices.Equal(facets[vid(1, 0)], ids(sub, tag, now)) || len(facets[vid(2, 0)]) != 0 {
		t.Fatalf("after delete %v", facets)
	}
	// A window opening is picked up by the window job.
	f.exec(`UPDATE {s}.content_memberships SET active_from = now() - interval '1 minute' WHERE group_id = $1`, string(later.ID))
	if err := f.s.RefreshWindows(f.ctx, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if facets := f.facetGroups(); !slices.Contains(facets[vid(1, 0)], string(later.ID)) {
		t.Fatalf("opened window %v", facets)
	}
	// The popularity catalog sees the same groups, per content and kind.
	byContent, err := f.s.Memberships(f.ctx, tenant, "gallery", "rail", []string{cid(1), cid(2)})
	if err != nil || !slices.Equal(byContent[cid(1)], []GroupID{now.ID, later.ID}) && !slices.Equal(byContent[cid(1)], []GroupID{later.ID, now.ID}) || len(byContent) != 1 {
		t.Fatalf("popularity memberships %v %v", byContent, err)
	}
}

// Memberships follow their group kind: version level, roles, positions,
// windows, and the content kinds that join it.
func TestMembershipRules(t *testing.T) {
	f := newFixture(t)
	f.content("gallery", 1, live("en"), live("es"))
	f.content("gallery", 2, live("en"))
	f.content("video", 3, live("en", "ja"))
	g := f.create(GroupInput{Kind: "tag", Slug: "t"}, GroupInput{Kind: "artist", Slug: "a"}, GroupInput{Kind: "series", Slug: "s"},
		GroupInput{Kind: "rail", Slug: "r"}, GroupInput{Kind: "voice_actor", Slug: "va"})
	tag, artist, series, rail, va := g[0], g[1], g[2], g[3], g[4]
	until := time.Now().Add(time.Hour)
	for name, m := range map[string]Membership{
		"version of a content-level kind": {GroupID: artist.ID, ContentID: cid(1), VersionID: vid(1, 0)},
		"undeclared role":                 {GroupID: artist.ID, ContentID: cid(1), Role: "translator"},
		"position in an unordered group":  {GroupID: tag.ID, ContentID: cid(1), Position: ptr(1)},
		"window on a series":              {GroupID: series.ID, ContentID: cid(1), ActiveUntil: &until},
		"kind the content does not join":  {GroupID: artist.ID, ContentID: cid(3)},
		"state":                           {GroupID: tag.ID, ContentID: cid(1), State: "maybe"},
	} {
		if err := f.s.AddMemberships(f.ctx, []Membership{m}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, m := range map[string]Membership{
		"another content's version": {GroupID: va.ID, ContentID: cid(2), VersionID: vid(1, 0)},
		"missing content":           {GroupID: tag.ID, ContentID: cid(9)},
		"missing group":             {GroupID: GroupID(cid(99)), ContentID: cid(1)},
	} {
		if err := f.s.AddMemberships(f.ctx, []Membership{m}); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	f.add(Membership{GroupID: artist.ID, ContentID: cid(1)}, Membership{GroupID: artist.ID, ContentID: cid(1), Role: "publisher"},
		Membership{GroupID: rail.ID, ContentID: cid(2), ActiveUntil: &until}, Membership{GroupID: tag.ID, ContentID: cid(3)})

	// Ordered groups append, keep existing positions and take explicit ones.
	f.add(Membership{GroupID: series.ID, ContentID: cid(2)}, Membership{GroupID: series.ID, ContentID: cid(1)})
	f.add(Membership{GroupID: series.ID, ContentID: cid(2)})
	page, err := f.s.Members(f.ctx, series.ID, MemberOptions{})
	if err != nil || len(page.Members) != 2 || page.Members[0].ContentID != cid(2) || *page.Members[0].Position != 1 || *page.Members[1].Position != 2 {
		t.Fatalf("appended %+v %v", page, err)
	}
	if err := f.s.Reorder(f.ctx, []Order{{GroupID: series.ID, Members: []MemberRef{{ContentID: cid(1)}}}}); err != nil {
		t.Fatal(err)
	}
	if page, _ = f.s.Members(f.ctx, series.ID, MemberOptions{}); page.Members[0].ContentID != cid(1) || *page.Members[1].Position != 2 {
		t.Fatalf("reordered %+v", page)
	}
	if err := f.s.Reorder(f.ctx, []Order{{GroupID: tag.ID}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reorder an unordered group: %v", err)
	}
	if err := f.s.Reorder(f.ctx, []Order{{GroupID: series.ID, Members: []MemberRef{{ContentID: cid(3)}}}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reorder a non-member: %v", err)
	}

	// A content's set replaces its memberships in the kinds it names only.
	if err := f.s.SetContentGroups(f.ctx, []ContentGroups{{ContentID: cid(1), Kinds: []string{"artist", "tag", "voice_actor"}, Memberships: []Membership{
		{GroupID: tag.ID}, {GroupID: va.ID, VersionID: vid(1, 1)}, {GroupID: artist.ID, Role: "publisher"}}}}); err != nil {
		t.Fatal(err)
	}
	held, err := f.s.MembershipsOf(f.ctx, []string{cid(1)})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range held[cid(1)] {
		got = append(got, fmt.Sprintf("%s/%s/%s", m.GroupID, m.VersionID, m.Role))
	}
	want := []string{fmt.Sprintf("%s//publisher", artist.ID), fmt.Sprintf("%s//", series.ID), fmt.Sprintf("%s//", tag.ID), fmt.Sprintf("%s/%s/", va.ID, vid(1, 1))}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("after set\n %v\nwant\n %v", got, want)
	}
	if err := f.s.SetContentGroups(f.ctx, []ContentGroups{{ContentID: cid(1), Kinds: []string{"tag"}, Memberships: []Membership{{GroupID: artist.ID}}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("membership outside the kinds written: %v", err)
	}
	if err := f.s.SetContentGroups(f.ctx, []ContentGroups{{ContentID: cid(3), Kinds: []string{"artist"}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("kind the content does not join: %v", err)
	}
	// With no kinds, every kind gallery joins is replaced: the series membership goes.
	if err := f.s.SetContentGroups(f.ctx, []ContentGroups{{ContentID: cid(1)}}); err != nil {
		t.Fatal(err)
	}
	if held, _ = f.s.MembershipsOf(f.ctx, []string{cid(1)}); len(held[cid(1)]) != 0 {
		t.Fatalf("emptied %+v", held)
	}
	if err := f.s.RemoveMemberships(f.ctx, []Membership{{GroupID: rail.ID, ContentID: cid(2)}}); err != nil {
		t.Fatal(err)
	}
	if held, _ = f.s.MembershipsOf(f.ctx, []string{cid(2), cid(3)}); len(held[cid(2)]) != 1 || len(held[cid(3)]) != 1 {
		t.Fatalf("after remove %+v", held)
	}
}

// Merging moves memberships (an accepted one wins over a proposed duplicate),
// names, edges, children and scoped groups, and redirects the code.
func TestMerge(t *testing.T) {
	f := newFixture(t)
	f.content("gallery", 1, live("en"))
	f.content("gallery", 2, live("en"))
	g := f.create(
		GroupInput{Kind: "series", Slug: "a", Names: []Name{canonical("en", "Alpha")}},
		GroupInput{Kind: "series", Slug: "b", Names: []Name{canonical("en", "Beta")}, Text: []Text{{Language: "es", Description: "b"}}},
		GroupInput{Kind: "series", Slug: "other"},
	)
	a, b, other := g[0], g[1], g[2]
	child := f.create(GroupInput{Kind: "series", Slug: "child", ParentID: b.ID})[0]
	char := f.create(GroupInput{Kind: "character", Slug: "hero", ScopeID: b.ID})[0]
	f.add(Membership{GroupID: b.ID, ContentID: cid(1)}, Membership{GroupID: a.ID, ContentID: cid(1), State: MembershipProposed}, Membership{GroupID: b.ID, ContentID: cid(2)})
	if err := f.s.AddEdges(f.ctx, []Edge{{From: b.ID, Relation: "related", To: other.ID}, {From: other.ID, Relation: "sequel_of", To: b.ID}, {From: a.ID, Relation: "related", To: b.ID}}); err != nil {
		t.Fatal(err)
	}
	reports, err := f.s.Merge(f.ctx, []Merge{{From: b.ID, Into: a.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if r := reports[0]; r.MembershipsMoved != 1 || r.NamesMoved != 1 || r.EdgesMoved != 2 || r.GroupsMoved != 2 {
		t.Fatalf("report %+v", r)
	}
	members, _ := f.s.Members(f.ctx, a.ID, MemberOptions{})
	if len(members.Members) != 2 || members.Members[0].State != MembershipActive {
		t.Fatalf("members %+v", members)
	}
	d, err := f.s.Detail(f.ctx, b.ID, "en")
	if err != nil || d.ID != a.ID || len(d.Names) != 2 || len(d.Children) != 1 || d.Children[0].ID != child.ID || len(d.Edges) != 2 || len(d.Text) != 1 {
		t.Fatalf("merged detail resolves to the survivor: %+v %v", d, err)
	}
	moved, _ := f.s.Groups(f.ctx, []GroupID{char.ID, b.ID})
	if moved[0].ScopeID != a.ID || moved[1].State != StateMerged || moved[1].MergedInto != a.ID {
		t.Fatalf("moved %+v", moved)
	}
	if facets := f.facetGroups(); !slices.Equal(facets[vid(1, 0)], ids(a)) || !slices.Equal(facets[vid(2, 0)], ids(a)) {
		t.Fatalf("facets %v", facets)
	}
	urls, _ := contenturl.New(contenturl.Options{Pool: f.pool, Schema: f.schema, Tenant: tenant})
	links, err := urls.Links(f.ctx, []contentref.ContentRef{contentref.New(tenant, "series", string(a.ID)), contentref.New(tenant, "series", string(b.ID))})
	if err != nil || len(links) != 2 || links[contentref.New(tenant, "series", string(b.ID)).Key()].Code != links[contentref.New(tenant, "series", string(a.ID)).Key()].Code {
		t.Fatalf("code redirect %+v %v", links, err)
	}

	for name, m := range map[string]Merge{"itself": {From: a.ID, Into: a.ID}} {
		if _, err := f.s.Merge(f.ctx, []Merge{m}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	tag := f.create(GroupInput{Kind: "tag", Slug: "t"})[0]
	chans := f.create(GroupInput{Kind: "channel", Slug: "c1", OwnerUserID: "u"}, GroupInput{Kind: "channel", Slug: "c2", OwnerUserID: "u"})
	f.exec(`UPDATE {s}.content_groups SET ever_sold = true WHERE id = $1`, string(other.ID))
	for name, m := range map[string]Merge{"merged": {From: b.ID, Into: a.ID}, "other kind": {From: tag.ID, Into: a.ID},
		"user-owned": {From: chans[0].ID, Into: chans[1].ID}, "sold": {From: other.ID, Into: a.ID}} {
		if _, err := f.s.Merge(f.ctx, []Merge{m}); !errors.Is(err, ErrConflict) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Counts are distinct live, listed contents per group, content kind and
// language ("" for all), descendants included; the automatic cover is the
// most-favorited live member, a selected cover stays.
func TestRecount(t *testing.T) {
	f := newFixture(t)
	f.content("gallery", 1, live("en"), live("es"))
	f.content("gallery", 2, live("en"), version{languages: []string{"ja"}, favorites: 50})
	f.content("gallery", 3, version{languages: []string{"en"}, live: &past, unlisted: true})
	f.content("video", 4, version{languages: []string{"en", "ja"}, live: &past, favorites: 7})
	g := f.create(GroupInput{Kind: "series", Slug: "parent"}, GroupInput{Kind: "tag", Slug: "tag"}, GroupInput{Kind: "tag", Slug: "picked", Cover: &Cover{ContentID: cid(1)}})
	parent, tag, picked := g[0], g[1], g[2]
	child := f.create(GroupInput{Kind: "series", Slug: "child", ParentID: parent.ID})[0]
	f.exec(`UPDATE {s}.content_facets SET favorites = 3 WHERE version_id = $1`, vid(1, 1))
	f.add(Membership{GroupID: child.ID, ContentID: cid(1)}, Membership{GroupID: parent.ID, ContentID: cid(2)}, Membership{GroupID: parent.ID, ContentID: cid(3)},
		Membership{GroupID: tag.ID, ContentID: cid(1)}, Membership{GroupID: tag.ID, ContentID: cid(4)}, Membership{GroupID: picked.ID, ContentID: cid(2)})
	if err := f.s.Recount(f.ctx); err != nil {
		t.Fatal(err)
	}
	counts := func(id GroupID) string {
		d, err := f.s.Detail(f.ctx, id, "")
		if err != nil {
			t.Fatal(err)
		}
		s := ""
		for _, c := range d.Counts {
			s += fmt.Sprintf("%s/%s=%d ", c.ContentKind, c.Language, c.Count)
		}
		return s
	}
	if got := counts(parent.ID); got != "gallery/=2 gallery/en=2 gallery/es=1 " {
		t.Fatalf("parent %s", got)
	}
	if got := counts(tag.ID); got != "gallery/=1 gallery/en=1 gallery/es=1 video/=1 video/en=1 video/ja=1 " {
		t.Fatalf("tag %s", got)
	}
	covers, _ := f.s.Groups(f.ctx, []GroupID{tag.ID, picked.ID, child.ID})
	if c := covers[0].Cover; c == nil || c.ContentID != cid(4) || !c.Auto {
		t.Fatalf("auto cover %+v", c)
	}
	if c := covers[1].Cover; c == nil || c.ContentID != cid(1) || c.Auto {
		t.Fatalf("picked cover %+v", c)
	}
	if c := covers[2].Cover; c == nil || c.ContentID != cid(1) {
		t.Fatalf("child cover %+v", c)
	}
	// A deleted group loses its counts; a removed member stops counting.
	if _, err := f.s.DeleteGroups(f.ctx, []GroupID{child.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RemoveMemberships(f.ctx, []Membership{{GroupID: tag.ID, ContentID: cid(4)}}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Recount(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := counts(parent.ID); got != "gallery/=1 gallery/en=1 " {
		t.Fatalf("parent after %s", got)
	}
	if got := counts(tag.ID); got != "gallery/=1 gallery/en=1 gallery/es=1 " {
		t.Fatalf("tag after %s", got)
	}
	var n int
	if err := f.pool.QueryRow(f.ctx, fmt.Sprintf(`SELECT count(*) FROM %q.content_group_counts WHERE group_id = $1`, f.schema), string(child.ID)).Scan(&n); err != nil || n != 0 {
		t.Fatalf("deleted group keeps %d counts %v", n, err)
	}
}

// A group counts the content it owns as well as its members, each item once
// per language it serves and once overall, never once per version or twice
// for being both.
func TestRecountOwnedContent(t *testing.T) {
	f := newFixture(t)
	ch := f.create(GroupInput{Kind: "channel", Slug: "studio", OwnerUserID: "owner"})[0]
	f.content("video", 1, live("en"), live("es"))
	f.content("video", 2, version{languages: []string{"en", "es"}, live: &past})
	f.content("video", 3, live("en"))
	f.exec(`UPDATE {s}.content_facets SET owner_group_id = $1 WHERE content_id = ANY($2)`, string(ch.ID), []string{cid(1), cid(2)})
	f.add(Membership{GroupID: ch.ID, ContentID: cid(2)}, Membership{GroupID: ch.ID, ContentID: cid(3)})
	solo := f.create(GroupInput{Kind: "channel", Slug: "solo", OwnerUserID: "owner"})[0]
	f.content("video", 4, live("en"))
	f.exec(`UPDATE {s}.content_facets SET owner_group_id = $1 WHERE content_id = $2`, string(solo.ID), cid(4))
	f.exec(`UPDATE {s}.content_facets SET favorites = 5 WHERE content_id = $1`, cid(1))
	if err := f.s.Recount(f.ctx); err != nil {
		t.Fatal(err)
	}
	// Automatic covers pick from owned content too: a channel with no members has one.
	covers, err := f.s.Groups(f.ctx, []GroupID{ch.ID, solo.ID})
	if err != nil {
		t.Fatal(err)
	}
	if c := covers[0].Cover; c == nil || c.ContentID != cid(1) || !c.Auto {
		t.Fatalf("channel cover %+v", c)
	}
	if c := covers[1].Cover; c == nil || c.ContentID != cid(4) || !c.Auto {
		t.Fatalf("owned-only channel cover %+v", c)
	}
	d, err := f.s.Detail(f.ctx, ch.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	got := ""
	for _, c := range d.Counts {
		got += fmt.Sprintf("%s/%s=%d ", c.ContentKind, c.Language, c.Count)
	}
	if got != "video/=3 video/en=3 video/es=2 " {
		t.Fatalf("counts %s", got)
	}
	page, err := f.s.List(f.ctx, ListOptions{Kind: "channel", Slug: "studio"})
	if err != nil || len(page.Groups) != 1 || page.Groups[0].Count != 3 {
		t.Fatalf("card %+v %v", page.Groups, err)
	}
}

// Listings filter, resolve names in the request language and page every
// order by cursor without repeating or dropping a group.
func TestList(t *testing.T) {
	f := newFixture(t)
	f.content("gallery", 1, live("en"))
	f.content("gallery", 2, live("es"))
	var in []GroupInput
	for i, n := range []string{"delta", "Alpha", "charlie", "bravo", "echo", "Écho2", "9lives"} {
		in = append(in, GroupInput{Kind: "tag", Slug: fmt.Sprintf("t%d", i), Names: []Name{canonical("en", n)}})
	}
	in = append(in, GroupInput{Kind: "tag", Slug: "spanish", Names: []Name{canonical("es", "Zeta")}, Text: []Text{{Language: "en", SortKey: "aaa"}}})
	tags := f.create(in...)
	f.add(Membership{GroupID: tags[0].ID, ContentID: cid(1)}, Membership{GroupID: tags[0].ID, ContentID: cid(2)}, Membership{GroupID: tags[4].ID, ContentID: cid(2)})
	if err := f.s.Recount(f.ctx); err != nil {
		t.Fatal(err)
	}
	walk := func(opts ListOptions) []string {
		t.Helper()
		var out []string
		for i := 0; ; i++ {
			page, err := f.s.List(f.ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, g := range page.Groups {
				out = append(out, g.Name)
			}
			if opts.Cursor = page.NextCursor; opts.Cursor == "" || i > 10 {
				return out
			}
		}
	}
	if got := walk(ListOptions{Kind: "tag", Language: "en", Limit: 3}); !slices.Equal(got, []string{"9lives", "Zeta", "Alpha", "bravo", "charlie", "delta", "echo", "Écho2"}) {
		t.Fatalf("by name %v", got)
	}
	if got := walk(ListOptions{Kind: "tag", Sort: SortCount, Limit: 2}); got[0] != "delta" || got[1] != "echo" || len(got) != 8 {
		t.Fatalf("by count %v", got)
	}
	if got := walk(ListOptions{Kind: "tag", Sort: SortNewest, Limit: 3}); len(got) != 8 || len(slices.Compact(slices.Sorted(slices.Values(got)))) != 8 {
		t.Fatalf("newest %v", got)
	}
	if got := walk(ListOptions{Kind: "tag", Language: "en", Letter: "e"}); !slices.Equal(got, []string{"echo", "Écho2"}) {
		t.Fatalf("letter e %v", got)
	}
	if got := walk(ListOptions{Kind: "tag", Language: "en", Letter: "#"}); !slices.Equal(got, []string{"9lives"}) {
		t.Fatalf("letter # %v", got)
	}
	page, err := f.s.List(f.ctx, ListOptions{Kind: "tag", Slug: "t0", Language: "es"})
	if err != nil || len(page.Groups) != 1 {
		t.Fatal(page, err)
	}
	if c := page.Groups[0]; c.Count != 1 || c.Code == "" || c.Path != "/es/tags/"+c.Code+"/delta" || c.NameLanguage != "en" {
		t.Fatalf("card %+v", c)
	}
	if page, _ = f.s.List(f.ctx, ListOptions{Kind: "tag", Slug: "t0"}); page.Groups[0].Count != 2 || page.Groups[0].Path != "/en/tags/"+page.Groups[0].Code+"/delta" {
		t.Fatalf("all languages %+v", page.Groups[0])
	}
	if page, _ = f.s.List(f.ctx, ListOptions{Kind: "tag", Slug: "t0", Language: "en"}); page.Groups[0].Path != "/en/tags/"+page.Groups[0].Code+"/delta" {
		t.Fatalf("the default language is prefixed too: %+v", page.Groups[0])
	}
	for name, opts := range map[string]ListOptions{"kind": {Kind: "studio"}, "sort": {Sort: "random"}, "letter": {Letter: "ab"}, "cursor": {Cursor: "!"}, "parent": {Parent: "x"}} {
		if _, err := f.s.List(f.ctx, opts); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Parent and scope filters; detail carries parent, scope and children.
	series := f.create(GroupInput{Kind: "series", Slug: "fate", Names: []Name{canonical("en", "Fate")}})[0]
	sub := f.create(GroupInput{Kind: "series", Slug: "zero", ParentID: series.ID, Names: []Name{canonical("en", "Zero")}},
		GroupInput{Kind: "character", Slug: "saber", ScopeID: series.ID, Names: []Name{canonical("en", "Saber")}})
	if got := walk(ListOptions{Parent: series.ID}); !slices.Equal(got, []string{"Zero"}) {
		t.Fatalf("children %v", got)
	}
	if got := walk(ListOptions{Kind: "character", Scope: series.ID}); !slices.Equal(got, []string{"Saber"}) {
		t.Fatalf("scoped %v", got)
	}
	d, err := f.s.Detail(f.ctx, series.ID, "en")
	if err != nil || len(d.Children) != 1 || d.Children[0].ID != sub[0].ID || d.Path == "" {
		t.Fatalf("detail %+v %v", d, err)
	}
	if d, err = f.s.Detail(f.ctx, sub[1].ID, "en"); err != nil || d.Scope == nil || d.Scope.Name != "Fate" || d.Parent != nil {
		t.Fatalf("character detail %+v %v", d, err)
	}
	if _, err := f.s.DeleteGroups(f.ctx, []GroupID{sub[1].ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Detail(f.ctx, sub[1].ID, "en"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted detail: %v", err)
	}
}

// Group kinds are keyword documents: one per language, titled by the
// language's canonical name, others as aliases; backfill lists active groups.
func TestKeywordDocuments(t *testing.T) {
	f := newFixture(t)
	g := f.create(GroupInput{Kind: "tag", Slug: "a", Names: []Name{canonical("en", "Colored"), alias("en", "Colour"), canonical("es", "Coloreado")}},
		GroupInput{Kind: "tag", Slug: "b"}, GroupInput{Kind: "artist", Slug: "c", Names: []Name{canonical("en", "Shindol")}})
	if _, err := f.s.DeleteGroups(f.ctx, []GroupID{g[2].ID}); err != nil {
		t.Fatal(err)
	}
	refs := []contentref.ContentRef{contentref.New(tenant, "tag", string(g[0].ID)), contentref.New(tenant, "tag", string(g[1].ID))}
	docs, err := f.s.BuildDocuments(f.ctx, "tag", "es", refs)
	if err != nil || len(docs) != 1 || docs[0].Title != "Coloreado" || !slices.Equal(docs[0].Aliases, []string{"Colored", "Colour"}) {
		t.Fatalf("documents %+v %v", docs, err)
	}
	if _, err := f.s.BuildDocuments(f.ctx, "gallery", "en", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("content kind: %v", err)
	}
	listed, next, done, err := f.s.ListDocuments(f.ctx, "artist", "en", "", 10)
	if err != nil || len(listed) != 0 || next != "" || !done {
		t.Fatalf("deleted artist listed: %v %v", listed, err)
	}
	listed, _, done, err = f.s.ListDocuments(f.ctx, "tag", "en", "", 1)
	if err != nil || len(listed) != 1 || done {
		t.Fatalf("tag page %v %v %v", listed, done, err)
	}
}
