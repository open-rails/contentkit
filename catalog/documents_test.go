package catalog

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/entities"

	"github.com/open-rails/contentkit/internal/worker"
	"github.com/open-rails/contentkit/search"
)

// Content items are their own search documents: the worker builds one per
// item and language from its text, with its entities' names as keywords, and
// searches keep only what the viewer's listing shows.
func TestSearchDocumentsIntegration(t *testing.T) {
	f := newFixture(t, nil)
	tag := f.entity("tag", "seaside")
	live := f.create("staff", ContentInput{Kind: "gallery", Text: map[string]Text{"en": {Title: "Blue Ocean", Aliases: []string{"Azure Sea"}}, "ja": {Title: "青い海"}},
		Versions: []VersionInput{{Languages: []string{"en"}}}})
	f.publish("staff", live, nil)
	f.join(tag, live, "")
	draft := f.create("staff", gallery("Blue Draft", "en"))
	premium := f.create("staff", ContentInput{Kind: "gallery", Text: map[string]Text{"en": {Title: "Blue Premium"}}, Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}, Versions: []VersionInput{{Languages: []string{"en"}}}})
	f.publish("staff", premium, nil)
	f.claim("pam", "premium")
	opts := worker.Options{Pool: f.pool, Schema: f.schema, Tenant: tenant, SupportedLanguages: f.s.Kinds().Languages, Sources: []worker.Source{f.s}}
	sync := func() {
		t.Helper()
		for range 3 {
			if err := worker.SyncOnce(f.ctx, opts); err != nil {
				t.Fatal(err)
			}
		}
	}
	sync()
	find := func(q, language, actor string) []string {
		t.Helper()
		// The listing predicate joined to each document, as discovery's search does.
		pred, args := f.viewer(actor, "main.test", ViewerOptions{}).Listed("f")
		join := &search.Eligibility{SQL: `SELECT 0 AS priority WHERE EXISTS (SELECT 1 FROM ` + f.table("content_facets") +
			` f WHERE f.tenant_id = sd.tenant_id AND f.content_id = sd.content_id AND ` + pred + `)`, Args: args}
		res, err := search.KeywordSearch(f.ctx, f.pool, q, search.Options{Schema: f.schema, Tenant: tenant, Language: language, Limit: 20, Eligibility: join})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, h := range res.Hits {
			if !slices.Contains(out, h.ContentID) {
				out = append(out, h.ContentID)
			}
		}
		slices.Sort(out)
		return out
	}
	if got := find("blue", "en", anon); !slices.Equal(got, sorted(live, premium)) {
		t.Fatalf("anonymous finds %v, want both published items (premium remains locked)", got)
	}
	if got := find("blue", "en", "pam"); !slices.Equal(got, sorted(live, premium)) {
		t.Fatalf("a tier holder finds %v", got)
	}
	if got := find("blue", "en", "staff"); !slices.Equal(got, sorted(live, premium)) {
		t.Fatalf("staff find %v (drafts have no document)", got)
	}
	if got := find("azure sea", "en", anon); !slices.Equal(got, []string{live}) {
		t.Fatalf("an alias finds %v", got)
	}
	if got := find("seaside", "en", anon); !slices.Equal(got, []string{live}) {
		t.Fatalf("a group's name finds %v", got)
	}
	if got := find("青い海", "ja", anon); !slices.Equal(got, []string{live}) {
		t.Fatalf("Japanese text finds %v", got)
	}

	// A renamed entity's members, inherited ones through a parent included,
	// are found by the new name only, whichever write renames it.
	coast := f.entity("tag", "coast")
	parent := entities.EntityID(coast)
	if _, err := f.entities.UpdateEntities(f.ctx, []entities.EntityUpdate{{ID: entities.EntityID(tag), ParentID: &parent}}); err != nil {
		t.Fatal(err)
	}
	series := f.entity("series", "voyage")
	f.join(series, live, "")
	sync()
	renamed := func(old, now string) {
		t.Helper()
		sync()
		if got := find(old, "en", anon); len(got) != 0 {
			t.Fatalf("the old name %q still finds %v", old, got)
		}
		if got := find(now, "en", anon); !slices.Equal(got, []string{live}) {
			t.Fatalf("the new name %q finds %v", now, got)
		}
	}
	name := func(s string) []entities.Name {
		return []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: s}}
	}
	if err := f.entities.SetNames(f.ctx, []entities.EntityNames{{ID: entities.EntityID(tag), Names: name("Mountain")}}); err != nil {
		t.Fatal(err)
	}
	renamed("seaside", "mountain")
	if _, err := f.entities.UpdateEntities(f.ctx, []entities.EntityUpdate{{ID: parent, Names: name("Harbor")}}); err != nil {
		t.Fatal(err)
	}
	renamed("coast", "harbor")
	// Resending the same names and slug queues none of the members.
	slug := "coast"
	if _, err := f.entities.UpdateEntities(f.ctx, []entities.EntityUpdate{{ID: parent, Slug: &slug, Names: name("Harbor")}}); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+pgx.Identifier{f.schema, "content_search_dirty"}.Sanitize()+` WHERE content_id = $1`, live).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("an unchanged update queued %d documents: %v", queued, err)
	}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.entities.ImportEntities(f.ctx, tx, []entities.ImportEntity{{ID: entities.EntityID(tag), Kind: "tag", Slug: "seaside", ParentID: parent,
		State: entities.StateActive, Names: name("Summit")}}, false); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	renamed("mountain", "summit")
	// A path entity's rename already re-derived its members; it still does.
	if err := f.entities.SetNames(f.ctx, []entities.EntityNames{{ID: entities.EntityID(series), Names: name("Odyssey")}}); err != nil {
		t.Fatal(err)
	}
	renamed("voyage", "odyssey")

	// Edits rebuild; deletion and a removed language drop the documents.
	if err := f.s.PutText(f.ctx, f.as("staff"), []TextPut{{ContentID: live, Language: "en", Text: &Text{Title: "Green Ocean"}}, {ContentID: live, Language: "ja"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteForModeration(f.ctx, f.as("staff"), []string{premium}); err != nil {
		t.Fatal(err)
	}
	sync()
	if got := find("blue", "en", "staff"); len(got) != 0 {
		t.Fatalf("stale documents %v", got)
	}
	if got := find("green", "en", anon); !slices.Equal(got, []string{live}) {
		t.Fatalf("the rebuilt document %v", got)
	}
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.table("content_search_documents")+` WHERE content_id = ANY($1)`, []string{draft, premium}).Scan(&n); err != nil || n != 0 {
		t.Fatalf("documents of a draft and a deleted item: %d %v", n, err)
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.table("content_search_documents")+` WHERE content_id = $1 AND language = 'ja'`, live).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a removed language's document: %d %v", n, err)
	}

	// Backfill lists published items with text in each language.
	refs, _, done, err := f.s.ListDocuments(f.ctx, "gallery", "en", "", 10)
	if err != nil || !done || len(refs) != 1 || refs[0].ContentID != live {
		t.Fatalf("ListDocuments %v %v %v", refs, done, err)
	}
}
