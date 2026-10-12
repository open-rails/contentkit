package catalog

import (
	"github.com/open-rails/contentkit/access"
	"slices"
	"testing"

	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/internal/worker"
)

// Content items are their own search documents: the worker builds one per
// item and language from its text, with its groups' names as keywords, and
// searches keep only what the viewer's listing shows.
func TestSearchDocumentsIntegration(t *testing.T) {
	f := newFixture(t, nil)
	tag := f.group("tag", "seaside")
	live := f.create("staff", ContentInput{Kind: "gallery", Text: map[string]Text{"en": {Title: "Blue Ocean", Aliases: []string{"Azure Sea"}}, "ja": {Title: "青い海"}},
		Versions: []VersionInput{{Languages: []string{"en"}}}})
	f.publish("staff", live, nil)
	f.join(tag, live, "")
	draft := f.create("staff", gallery("Blue Draft", "en"))
	premium := f.create("staff", ContentInput{Kind: "gallery", Text: map[string]Text{"en": {Title: "Blue Premium"}}, Listing: ListingMembers,
		Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}, Versions: []VersionInput{{Languages: []string{"en"}}}})
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
	if got := find("blue", "en", anon); !slices.Equal(got, []string{live}) {
		t.Fatalf("anonymous finds %v, want the live item only", got)
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

	// Edits rebuild; deletion and a removed language drop the documents.
	if err := f.s.PutText(f.ctx, f.as("staff"), []TextPut{{ContentID: live, Language: "en", Text: &Text{Title: "Green Ocean"}}, {ContentID: live, Language: "ja"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Delete(f.ctx, f.as("staff"), []string{premium}); err != nil {
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
