package catalog

import "testing"

func TestBackfillVersionTitlesSameLanguageEditions(t *testing.T) {
	f := newFixture(t, nil)
	id := f.create("staff", ContentInput{Kind: "gallery", Text: map[string]Text{"en": {Title: "Known English title"}},
		Versions: []VersionInput{{Languages: []string{"en"}}, {Languages: []string{"en"}}},
	})
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM `+f.table("content_version_text")+` WHERE tenant_id=$1 AND version_id IN (SELECT id FROM `+f.table("content_versions")+` WHERE tenant_id=$1 AND content_id=$2)`, tenant, id); err != nil {
		t.Fatal(err)
	}
	changed, err := BackfillVersionTitles(f.ctx, f.pool, f.schema, tenant, "gallery")
	if err != nil || changed != 2 {
		t.Fatalf("two edition titles must migrate atomically: changed=%d err=%v", changed, err)
	}
	var titles, documents int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.table("content_version_text")+` t JOIN `+f.table("content_versions")+` v ON v.tenant_id=t.tenant_id AND v.id=t.version_id WHERE v.tenant_id=$1 AND v.content_id=$2 AND t.language='en' AND t.title='Known English title'`, tenant, id).Scan(&titles); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.table("content_search_dirty")+` WHERE tenant_id=$1 AND content_id=$2 AND language='en' AND reason='version_titles'`, tenant, id).Scan(&documents); err != nil {
		t.Fatal(err)
	}
	if titles != 2 || documents != 1 {
		t.Fatalf("titles=%d parent/language dirty documents=%d", titles, documents)
	}
	if again, err := BackfillVersionTitles(f.ctx, f.pool, f.schema, tenant, "gallery"); err != nil || again != 0 {
		t.Fatalf("rerun: changed=%d err=%v", again, err)
	}
}
