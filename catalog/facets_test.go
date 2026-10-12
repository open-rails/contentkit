package catalog

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type facet struct {
	Version      string
	Live         bool
	Deleted      bool
	Listing      string
	Languages    []string
	Level        string
	Tiers        []string
	Group        string
	Restrictions []string
	Groups       []string
	Traits       []string
	Owner        string
	Position     int
	Attributes   map[string]any
	Likes        int
	Favorites    int
}

// facets reads the content item's facet rows in position order.
func (f *fixture) facets(id string) []facet {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT version_id, coalesce(live_from <= $2, false), deleted, listing, languages, access_level, access_tiers,
 coalesce(access_group_id, ''), restrictions, group_ids, traits, coalesce(owner_group_id, ''), position, attributes, likes, favorites
 FROM `+f.table("content_facets")+` WHERE tenant_id = $1 AND content_id = $3 ORDER BY position, version_id`, tenant, f.now(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []facet
	for rows.Next() {
		var x facet
		var attrs []byte
		if err := rows.Scan(&x.Version, &x.Live, &x.Deleted, &x.Listing, &x.Languages, &x.Level, &x.Tiers, &x.Group, &x.Restrictions,
			&x.Groups, &x.Traits, &x.Owner, &x.Position, &attrs, &x.Likes, &x.Favorites); err != nil {
			f.t.Fatal(err)
		}
		if err := json.Unmarshal(attrs, &x.Attributes); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, x)
	}
	return out
}

// content_facets holds one row per version, rewritten in the transaction of
// every change to its inputs: the item's state, listing, access, restrictions
// and attributes, each version's state, languages, traits, access override,
// attributes and order, and memberships; deletion keeps the rows (holders
// still read them) marked deleted.
func TestFacetsFollowEveryChangeIntegration(t *testing.T) {
	f := newFixture(t, nil)
	studio, red, blue := f.group("channel", "studio"), f.group("tag", "red"), f.group("tag", "blue")
	in := gallery("Facets", "en", "ja")
	in.OwnerGroupID = studio
	in.Attributes = map[string]any{"format": "doujin"}
	id := f.create("staff", in)
	vs := f.facets(id)
	if len(vs) != 2 || vs[0].Live || vs[0].Listing != ListingListed || vs[0].Level != LevelPublic || vs[0].Owner != studio ||
		!slices.Equal(vs[0].Languages, []string{"en"}) || !slices.Equal(vs[1].Languages, []string{"ja"}) || vs[0].Attributes["format"] != "doujin" {
		t.Fatalf("a draft's facets %+v", vs)
	}
	en, ja := vs[0].Version, vs[1].Version
	f.publish("staff", id, nil)
	if vs := f.facets(id); !vs[0].Live || !vs[1].Live {
		t.Fatalf("published %+v", vs)
	}
	err := f.s.Update(f.ctx, f.as("staff"), []Update{{ID: id, ContentPatch: ContentPatch{Listing: ptr(ListingMembers),
		Access: &Rule{Level: LevelTier, Tiers: []string{"premium"}}, Restrictions: &[]string{"restricted"}, Attributes: map[string]any{"volume": 2}}}})
	if err != nil {
		t.Fatal(err)
	}
	vs = f.facets(id)
	if vs[0].Listing != ListingMembers || vs[0].Level != LevelTier || !slices.Equal(vs[0].Tiers, []string{"premium"}) ||
		!slices.Equal(vs[1].Restrictions, []string{"restricted"}) || vs[1].Attributes["volume"] != float64(2) || vs[1].Attributes["format"] != nil {
		t.Fatalf("after the item's edit %+v", vs)
	}
	_, err = f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionID: ja, VersionInput: VersionInput{Traits: &[]string{"uncensored"},
		Access: &Rule{Level: LevelGroup, GroupID: studio}, Attributes: map[string]any{"note": "ja only"}}}})
	if err != nil {
		t.Fatal(err)
	}
	vs = f.facets(id)
	if vs[1].Level != LevelGroup || vs[1].Group != studio || !slices.Equal(vs[1].Traits, []string{"uncensored"}) ||
		vs[1].Attributes["note"] != "ja only" || vs[1].Attributes["volume"] != float64(2) || vs[0].Level != LevelTier {
		t.Fatalf("after the version's edit %+v", vs)
	}
	if _, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionID: ja, VersionInput: VersionInput{ClearAccess: true, State: StateDraft}}}); err != nil {
		t.Fatal(err)
	}
	if vs := f.facets(id); vs[1].Level != LevelTier || vs[1].Live {
		t.Fatalf("a cleared override inherits and a draft version is not live: %+v", vs[1])
	}
	if err := f.s.OrderVersions(f.ctx, f.as("staff"), id, []string{ja, en}); err != nil {
		t.Fatal(err)
	}
	if vs := f.facets(id); vs[0].Version != ja || vs[1].Version != en {
		t.Fatalf("after ordering %+v", vs)
	}
	f.join(red, id, "")
	f.join(blue, id, ja)
	vs = f.facets(id)
	if !slices.Equal(vs[0].Groups, sorted(red, blue)) || !slices.Equal(vs[1].Groups, []string{red}) {
		t.Fatalf("memberships %+v", vs)
	}
	if err := f.s.DeleteVersions(f.ctx, f.as("staff"), []VersionKey{{ContentID: id, VersionID: ja}}); err != nil {
		t.Fatal(err)
	}
	if vs := f.facets(id); !vs[0].Deleted || vs[1].Deleted {
		t.Fatalf("a deleted version %+v", vs)
	}
	if err := f.s.DeleteVersions(f.ctx, f.as("staff"), []VersionKey{{ContentID: id, VersionID: en}}); err == nil {
		t.Fatal("the last version was deleted")
	}
	if err := f.s.Delete(f.ctx, f.as("staff"), []string{id}); err != nil {
		t.Fatal(err)
	}
	if vs := f.facets(id); !vs[0].Deleted || !vs[1].Deleted || !vs[1].Live {
		t.Fatalf("a deleted item keeps when it was live: %+v", vs)
	}

	// A failed write leaves the facets as they were.
	if err := f.s.Restore(f.ctx, f.as("staff"), []string{id}); err != nil {
		t.Fatal(err)
	}
	before := f.facets(id)
	if err := f.s.Update(f.ctx, f.as("staff"), []Update{{ID: id, ContentPatch: ContentPatch{Listing: ptr(ListingUnlisted), Revision: ptr(int64(1))}}}); err == nil {
		t.Fatal("a stale revision was accepted")
	}
	if after := f.facets(id); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused write moved the facets %+v -> %+v", before, after)
	}

	// Interaction counts reach the facets by the counts job.
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO `+f.table("content_interaction_counts")+` (tenant_id, content_kind, content_id, likes, dislikes, favorites)
 VALUES ($1, 'gallery', $2, 9, 1, 4)`, tenant, id); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.RefreshCounts(f.ctx); err != nil || n != 2 {
		t.Fatalf("RefreshCounts: %d %v", n, err)
	}
	var rating float64
	if err := f.pool.QueryRow(f.ctx, `SELECT rating FROM `+f.table("content_facets")+` WHERE content_id = $1 LIMIT 1`, id).Scan(&rating); err != nil || rating < 0.5 || rating > 0.9 {
		t.Fatalf("rating %v %v", rating, err)
	}
	if vs := f.facets(id); vs[0].Likes != 9 || vs[1].Favorites != 4 {
		t.Fatalf("counts %+v", vs)
	}
	if n, err := f.s.RefreshCounts(f.ctx); err != nil || n != 0 {
		t.Fatalf("an unchanged refresh wrote %d (%v)", n, err)
	}
	// A rewrite of the facets keeps the counts.
	if err := pgx.BeginFunc(f.ctx, f.pool, func(tx pgx.Tx) error { return f.s.RefreshFacets(f.ctx, tx, []string{id}) }); err != nil {
		t.Fatal(err)
	}
	if vs := f.facets(id); vs[0].Likes != 9 {
		t.Fatalf("refresh lost the counts %+v", vs)
	}
}

// RefreshFacets reads effective groups in the writer's transaction: a
// membership the transaction then rolls back never reaches the facets.
func TestFacetsShareTheWritersTransactionIntegration(t *testing.T) {
	f := newFixture(t, nil)
	tag := f.group("tag", "tag")
	id := f.create("staff", gallery("Tx", "en"))
	rollback := pgx.BeginFunc(f.ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(f.ctx, `INSERT INTO `+f.table("content_memberships")+` (tenant_id, group_id, content_id) VALUES ($1, $2, $3)`, tenant, tag, id); err != nil {
			return err
		}
		if err := f.s.RefreshFacets(f.ctx, tx, []string{id}); err != nil {
			return err
		}
		var groups []string
		if err := tx.QueryRow(f.ctx, `SELECT group_ids FROM `+f.table("content_facets")+` WHERE content_id = $1`, id).Scan(&groups); err != nil || len(groups) != 1 {
			t.Errorf("inside the transaction %v %v", groups, err)
		}
		return context.Canceled
	})
	if rollback != context.Canceled {
		t.Fatal(rollback)
	}
	if vs := f.facets(id); len(vs[0].Groups) != 0 {
		t.Fatalf("a rolled back membership reached the facets %+v", vs)
	}
	_ = time.Now
}
