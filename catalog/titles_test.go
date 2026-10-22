package catalog

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/kinds"
)

func TestEditionTitleKeepsOriginalSeparate(t *testing.T) {
	v := &Version{Languages: []string{"en"}, Text: map[string]VersionText{
		"en": {Title: "RESORT LOVER"}, "ja": {Title: "A retained localized override"},
	}}
	if got := editionTitle(v, "ja", "原題", "Legacy title"); got != "RESORT LOVER" {
		t.Fatalf("one-language edition used another language's text: %q", got)
	}
	delete(v.Text, "en")
	if got := editionTitle(v, "en", "原題", "Legacy title"); got != "原題" {
		t.Fatal(got)
	}
	if got := editionTitle(v, "en", "", "Legacy title"); got != "Legacy title" {
		t.Fatal(got)
	}
	if title, language, err := cleanOriginalTitle(" 原題 ", "ja"); err != nil || title != "原題" || language != "ja" {
		t.Fatalf("original: %q %q %v", title, language, err)
	}
	if _, _, err := cleanOriginalTitle(strings.Repeat("a", maxTitle+1), ""); err == nil {
		t.Fatal("oversized original title accepted")
	}
}

func TestOriginalAndVersionTitlesPersistAndBackfillWithoutGuessing(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		kind := o.Kinds.Kinds["gallery"]
		kind.TitlePolicy = kinds.TitleOriginalAndVersion
		o.Kinds.Kinds["gallery"] = kind
	})
	id := f.create("staff", ContentInput{Kind: "gallery", OriginalTitle: "原題", OriginalLanguage: "ja",
		Text: map[string]Text{"en": {Title: "Legacy English"}, "ja": {Title: "既存の日本語"}},
		Versions: []VersionInput{
			{Languages: []string{"en"}, Text: map[string]VersionText{"en": {Title: "RESORT LOVER", Description: "Keep description"}}},
			{Languages: []string{"ja"}},
		},
	})
	f.publish("staff", id, nil)
	got := f.read("staff", "main.test", id)[0]
	if got.Title != "RESORT LOVER" || got.OriginalTitle != "原題" || got.OriginalLanguage != "ja" {
		t.Fatalf("title resolution: %+v", got)
	}
	for _, query := range []string{"RESORT", "原題"} {
		names, err := f.s.ManagementSearch(f.ctx, f.as("staff"), ManagementSearchQuery{Query: query, Language: "en", ContentKinds: []string{"gallery"}, IncludeContents: true})
		if err != nil || len(names.Contents) != 1 || names.Contents[0].ID != id {
			t.Fatalf("management lookup %q: %+v %v", query, names, err)
		}
	}
	documents, err := f.s.BuildDocuments(f.ctx, "gallery", "en", []contentref.ContentRef{f.s.Ref("gallery", id)})
	if err != nil || len(documents) != 1 || documents[0].Title != "RESORT LOVER" {
		t.Fatalf("public title document: %+v %v", documents, err)
	}
	// Simulate a pre-adoption edition whose title still exists only on the parent.
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM `+f.table("content_version_text")+` WHERE tenant_id=$1 AND version_id=$2`, tenant, got.Versions[1].ID); err != nil {
		t.Fatal(err)
	}
	var revisionBefore int64
	if err := f.pool.QueryRow(f.ctx, `SELECT revision FROM `+f.table("content_versions")+` WHERE tenant_id=$1 AND id=$2`, tenant, got.Versions[1].ID).Scan(&revisionBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM `+f.table("content_search_dirty")+` WHERE tenant_id=$1 AND content_id=$2`, tenant, id); err != nil {
		t.Fatal(err)
	}
	contentRevisionBefore := got.Revision
	n, err := BackfillVersionTitles(f.ctx, f.pool, f.schema, tenant, "gallery")
	if err != nil || n != 1 {
		t.Fatalf("backfill: %d %v", n, err)
	}
	got = f.read("staff", "main.test", id)[0]
	if got.Versions[0].Text["en"].Title != "RESORT LOVER" || got.Versions[0].Text["en"].Description != "Keep description" ||
		got.Versions[1].Text["ja"].Title != "既存の日本語" || got.OriginalTitle != "原題" {
		t.Fatalf("backfill overwrote or inferred text: %+v", got)
	}
	var revisionAfter int64
	if err := f.pool.QueryRow(f.ctx, `SELECT revision FROM `+f.table("content_versions")+` WHERE tenant_id=$1 AND id=$2`, tenant, got.Versions[1].ID).Scan(&revisionAfter); err != nil {
		t.Fatal(err)
	}
	if got.Revision != contentRevisionBefore+1 || revisionAfter != revisionBefore+1 {
		t.Fatalf("backfill did not invalidate the parent and edition: parent %d -> %d, edition %d -> %d", contentRevisionBefore, got.Revision, revisionBefore, revisionAfter)
	}
	var dirty int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.table("content_search_dirty")+` WHERE tenant_id=$1 AND content_id=$2 AND language='ja' AND reason='version_titles'`, tenant, id).Scan(&dirty); err != nil || dirty != 1 {
		t.Fatalf("backfill did not enqueue the changed search document: %d %v", dirty, err)
	}
	if again, err := BackfillVersionTitles(f.ctx, f.pool, f.schema, tenant, "gallery"); err != nil || again != 0 {
		t.Fatalf("backfill is not idempotent: %d %v", again, err)
	}
	before := got.Text
	original := "更新した原題"
	if err := f.s.Update(f.ctx, f.as("staff"), []Update{{ID: id, ContentPatch: ContentPatch{OriginalTitle: &original}}}); err != nil {
		t.Fatal(err)
	}
	got = f.read("staff", "main.test", id)[0]
	if got.OriginalTitle != original || got.Title != "RESORT LOVER" || !reflect.DeepEqual(got.Text, before) {
		t.Fatal("original edit changed an edition or localized content title")
	}

	if err := pgx.BeginFunc(f.ctx, f.pool, func(tx pgx.Tx) error {
		return f.s.Import(f.ctx, tx, []ImportContent{{ID: id, Kind: "gallery", State: StatePublished, Versions: []ImportVersion{
			{ID: got.Versions[0].ID, Languages: got.Versions[0].Languages, State: StatePublished},
			{ID: got.Versions[1].ID, Languages: got.Versions[1].Languages, State: StatePublished},
		}}})
	}); err != nil {
		t.Fatal(err)
	}
	kept := f.read("staff", "main.test", id)[0]
	if kept.OriginalTitle != original || kept.OriginalLanguage != "ja" {
		t.Fatal("import omission erased the source title")
	}

	unknown := f.create("staff", ContentInput{Kind: "gallery", Text: map[string]Text{"en": {Title: "English only"}}, Versions: []VersionInput{{Languages: []string{"en"}}}})
	if _, err := BackfillVersionTitles(f.ctx, f.pool, f.schema, tenant, "gallery"); err != nil {
		t.Fatal(err)
	}
	other := f.read("staff", "main.test", unknown)[0]
	if other.OriginalTitle != "" || other.OriginalLanguage != "" || other.Versions[0].Text["en"].Title != "English only" {
		t.Fatalf("English text was mislabeled as an original: %+v", other)
	}

	// New adopters need no legacy content_text row: the source belongs to
	// the work, while the translated display title belongs to its edition.
	versionOnly := f.create("staff", ContentInput{Kind: "gallery", OriginalTitle: "別の原題", OriginalLanguage: "ja",
		Versions: []VersionInput{{Languages: []string{"en"}, Text: map[string]VersionText{"en": {Title: "Only on the edition"}}}},
	})
	f.publish("staff", versionOnly, nil)
	if current := f.read("staff", "main.test", versionOnly)[0]; current.Title != "Only on the edition" || len(current.Text) != 0 {
		t.Fatalf("edition-only title needs a redundant content title: %+v", current)
	}
	documents, err = f.s.BuildDocuments(f.ctx, "gallery", "en", []contentref.ContentRef{f.s.Ref("gallery", versionOnly)})
	if err != nil || len(documents) != 1 || documents[0].Title != "Only on the edition" {
		t.Fatalf("edition-only public document: %+v %v", documents, err)
	}
	refs, _, _, err := f.s.ListDocuments(f.ctx, "gallery", "en", "", 100)
	if err != nil || !slices.ContainsFunc(refs, func(ref contentref.ContentRef) bool { return ref.ContentID == versionOnly }) {
		t.Fatalf("edition-only title missing from search backfill: %+v %v", refs, err)
	}
}
