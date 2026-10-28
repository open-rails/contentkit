package critical_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/jobhook"
	"github.com/open-rails/contentkit/kinds"
)

// A rename queues one row per entity; the search sync expands it to every
// linked edition and language in batches across runs.
func TestEntityRenameReindexesLinkedContent(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["tag"] = kinds.EntityKind{Route: "tags"}
		gallery := cfg.Content.Kinds["gallery"]
		gallery.Entities = []string{"tag"}
		cfg.Content.Kinds["gallery"] = gallery
	})
	q := func(table string) string { return pgx.Identifier{w.schema, table}.Sanitize() }
	const items, batch = 5, 2 // three batches
	setup := func(t *testing.T, a *application) (entities.Entity, []string) {
		t.Helper()
		admin := a.engine.Client()
		tags, err := admin.PostEntities(t.Context(), []entities.EntityInput{{Kind: "tag", Slug: "renamed",
			Names: []entities.Name{{Language: "en", Kind: "name", Name: "Before Needle"}}}})
		must(t, err)
		var ids []string
		var versions [][2]string
		for n := range items {
			title := fmt.Sprintf("Renamed gallery %d", n)
			item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "gallery", Versions: []catalog.VersionInput{
				{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: title}}},
				{Languages: []string{"de"}, Text: map[string]catalog.VersionText{"de": {Title: title + " de"}}}}})
			must(t, err)
			_, err = admin.PutContentEntitiesShared(t.Context(), item.ID, entities.ContentEntities{Memberships: []entities.Membership{{EntityID: tags[0].ID}}}, ck.PutContentEntitiesSharedQuery{})
			must(t, err)
			_, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
			must(t, err)
			ids = append(ids, item.ID)
			for _, v := range item.Versions {
				versions = append(versions, [2]string{item.ID, v.ID})
			}
		}
		for _, v := range versions {
			awaitEditionPublication(t, a, admin, v[0], v[1])
		}
		// Expansion pages in the database's order.
		rows, err := w.pool.Query(t.Context(), `SELECT DISTINCT content_id FROM `+q("content_facets")+` WHERE content_id = ANY($1) ORDER BY content_id`, ids)
		must(t, err)
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		must(t, err)
		if len(ids) != items {
			t.Fatalf("%d of %d galleries have facets", len(ids), items)
		}
		return tags[0], ids
	}
	// named counts the linked documents per entity name among their keywords.
	named := func(t *testing.T, ids []string) map[string]int {
		t.Helper()
		rows, err := w.pool.Query(t.Context(), `SELECT k, count(*) FROM `+q("content_search_documents")+` d CROSS JOIN LATERAL unnest(d.keywords) k
 WHERE d.content_kind = 'gallery' AND d.content_id = ANY($1) AND k LIKE '% Needle' GROUP BY k`, ids)
		must(t, err)
		out := map[string]int{}
		for rows.Next() {
			var k string
			var n int
			must(t, rows.Scan(&k, &n))
			out[k] = n
		}
		must(t, rows.Err())
		var docs int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+q("content_search_documents")+` WHERE content_kind = 'gallery' AND content_id = ANY($1)`, ids).Scan(&docs))
		out[""] = docs
		return out
	}
	expect := func(t *testing.T, got map[string]int, want map[string]int) {
		t.Helper()
		want[""] = items * 2
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("documents by entity name %v, want %v", got, want)
		}
	}
	type pending struct {
		Exists   bool
		Cursor   string
		Revision int64
	}
	queued := func(t *testing.T, entity entities.EntityID) pending {
		t.Helper()
		var p pending
		err := w.pool.QueryRow(t.Context(), `SELECT true, cursor, revision FROM `+q("content_search_entity_dirty")+` WHERE entity_id = $1`, string(entity)).Scan(&p.Exists, &p.Cursor, &p.Revision)
		if !errors.Is(err, pgx.ErrNoRows) {
			must(t, err)
		}
		return p
	}
	rename := func(t *testing.T, a *application, entity entities.EntityID, name string, patch bool) {
		t.Helper()
		names := []entities.Name{{Language: "en", Kind: "name", Name: name}}
		var err error
		if patch {
			_, err = a.engine.Client().PatchEntity(t.Context(), string(entity), entities.EntityUpdate{Names: names})
		} else {
			_, err = a.engine.Client().PutEntityNames(t.Context(), string(entity), names)
		}
		must(t, err)
	}

	t.Run("CK-J07-S23_rename_reindexes_every_linked_edition_in_batches_across_runs_and_a_second_rename_restarts", func(t *testing.T) {
		a := w.apps[0]
		jobhook.SearchExpansion(a.engine, batch, 1)
		publicationOnlyFleet(t, a) // the search sync runs only below
		tag, ids := setup(t, a)
		run := func() bool {
			t.Helper()
			more, err := jobhook.SearchSync(t.Context(), a.engine)
			must(t, err)
			return more
		}
		for n := 0; run(); n++ {
			if n == 20 {
				t.Fatal("initial indexing did not finish")
			}
		}
		expect(t, named(t, ids), map[string]int{"Before Needle": items * 2})

		rename(t, a, tag.ID, "After Needle", false)
		first := queued(t, tag.ID)
		if !first.Exists || first.Cursor != "" {
			t.Fatalf("rename queued %+v, want one row from the start", first)
		}
		var marked int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+q("content_search_dirty")+` WHERE content_kind = 'gallery' AND content_id = ANY($1)`, ids).Scan(&marked))
		if marked != 0 {
			t.Fatalf("the rename transaction marked %d item documents", marked)
		}
		for pass := 1; pass <= 2; pass++ {
			if !run() {
				t.Fatalf("pass %d reported no remaining expansion", pass)
			}
			expect(t, named(t, ids), map[string]int{"After Needle": pass * batch * 2, "Before Needle": (items - pass*batch) * 2})
			if got := queued(t, tag.ID); got.Cursor != ids[pass*batch-1] || got.Revision != first.Revision {
				t.Fatalf("pass %d left %+v, want cursor %s at revision %d", pass, got, ids[pass*batch-1], first.Revision)
			}
		}

		rename(t, a, tag.ID, "Final Needle", true)
		second := queued(t, tag.ID)
		if !second.Exists || second.Cursor != "" || second.Revision <= first.Revision {
			t.Fatalf("second rename left %+v, want a restarted row past revision %d", second, first.Revision)
		}
		passes := 1
		for run() {
			if passes++; passes > 10 {
				t.Fatal("expansion did not finish")
			}
		}
		if passes != 3 {
			t.Fatalf("restarted expansion took %d runs, want 3 batches of %d", passes, batch)
		}
		expect(t, named(t, ids), map[string]int{"Final Needle": items * 2})
		if got := queued(t, tag.ID); got.Exists {
			t.Fatalf("finished expansion kept %+v", got)
		}
	})

	t.Run("CK-J07-S23_the_search_job_continues_an_expansion_at_once", func(t *testing.T) {
		a := w.apps[1]
		jobhook.SearchExpansion(a.engine, batch, 1)
		startPublicationRuntime(t, a)
		tag, ids := setup(t, a)
		waitForWithin(t, 45*time.Second, func() bool {
			return named(t, ids)["Before Needle"] == items*2
		})
		jobs := pgx.Identifier{publicationRiverSchema(a), "river_job"}.Sanitize()
		var renamed time.Time
		must(t, w.pool.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&renamed))
		rename(t, a, tag.ID, "After Needle", false)
		// Each run expands one batch; the periodic backstop is minutes away.
		waitForWithin(t, 30*time.Second, func() bool {
			return named(t, ids)["After Needle"] == items*2 && !queued(t, tag.ID).Exists
		})
		expect(t, named(t, ids), map[string]int{"After Needle": items * 2})
		// River records the last run's completion shortly after its writes.
		runs := 0
		for deadline := time.Now().Add(10 * time.Second); runs < 3 && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+jobs+` WHERE kind = 'contentkit_search_sync' AND state = 'completed' AND finalized_at > $1`, renamed).Scan(&runs))
		}
		if runs < 3 {
			t.Fatalf("expansion finished in %d search runs, want one per batch", runs)
		}
	})
}
