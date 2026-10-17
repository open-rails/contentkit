package signal

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

// Forgetting purged content erases every subject's rows of a whole work, its
// co-engagement pairs, and of a single version; other works, versions and
// tenants keep theirs.
func TestIntegrationForgetContents(t *testing.T) {
	st, conn := freshStore(t)
	ctx := context.Background()
	a, b, c := cid(1), cid(2), cid(3)
	tables, err := st.activeSubjectTables(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seeded []string
	for _, table := range contentTables {
		if !slices.Contains(tables, table) {
			continue
		}
		seeded = append(seeded, table)
		for _, row := range [][3]string{{"t", a, "a1"}, {"t", a, "a2"}, {"t", b, "b1"}, {"t", b, "b2"}, {"other", a, "a1"}} {
			// A distinct subject per row: no sort key may collapse two of them.
			q := fmt.Sprintf("INSERT INTO %s.%s (tenant, content_kind, content_id, content_version_id, subject_kind, subject) VALUES (?, 'gallery', ?, ?, 'user', ?)", st.db, table)
			if err := conn.Exec(ctx, q, row[0], row[1], row[2], row[0]+row[2]); err != nil {
				t.Fatalf("seed %s: %v", table, err)
			}
		}
	}
	if err := conn.Exec(ctx, "INSERT INTO "+st.db+".engagement_pairs (tenant, content_kind_a, content_id_a, content_kind_b, content_id_b) VALUES ('t', 'gallery', ?, 'gallery', ?), ('t', 'gallery', ?, 'gallery', ?), ('t', 'gallery', ?, 'gallery', ?)",
		a, b, b, c, c, a); err != nil {
		t.Fatal(err)
	}
	forget := []ForgottenContent{{Kind: "gallery", ContentID: a}, {Kind: "gallery", ContentID: b, VersionID: "b2"}}
	for range 2 { // idempotent
		if err := st.ForgetContents(ctx, "t", forget); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range seeded {
		rows, err := conn.Query(ctx, fmt.Sprintf("SELECT tenant, content_id, content_version_id FROM %s.%s ORDER BY tenant, content_version_id", st.db, table))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var tenant, id, version string
			if err := rows.Scan(&tenant, &id, &version); err != nil {
				t.Fatal(err)
			}
			got = append(got, tenant+"/"+version)
		}
		rows.Close()
		if want := []string{"other/a1", "t/b1"}; !slices.Equal(got, want) {
			t.Errorf("%s after forgetting: %v, want %v", table, got, want)
		}
	}
	var pairs []string
	rows, err := conn.Query(ctx, "SELECT content_id_a, content_id_b FROM "+st.db+".engagement_pairs WHERE tenant = 't'")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var x, y string
		if err := rows.Scan(&x, &y); err != nil {
			t.Fatal(err)
		}
		pairs = append(pairs, x+">"+y)
	}
	rows.Close()
	if want := []string{b + ">" + c}; !slices.Equal(pairs, want) {
		t.Errorf("pairs after forgetting %s: %v, want %v", a, pairs, want)
	}
	if err := st.ForgetContents(ctx, "t", []ForgottenContent{{Kind: "gallery"}}); err == nil {
		t.Error("forgetting content without an id")
	}
}
