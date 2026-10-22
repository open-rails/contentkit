package discovery

import (
	"testing"
	"time"

	"github.com/open-rails/contentkit/kinds"
)

func TestCardsUseEditionThenOriginalThenLegacyTitle(t *testing.T) {
	model := siteConfig()
	g := model.Kinds["gallery"]
	g.TitlePolicy = kinds.TitleOriginalAndVersion
	model.Kinds["gallery"] = g
	f := newFixtureConfig(t, nil, "", model)
	f.seed(item{n: 1, titles: map[string]string{"en": "Legacy English", "ja": "以前の題名"}, age: time.Hour,
		versions: []ver{{langs: []string{"en"}}, {langs: []string{"ja"}}}})
	f.exec(`UPDATE {s}.content_items SET original_title='原題',original_language='ja' WHERE tenant_id=$1 AND id=$2`, tenant, f.cid(1))
	f.exec(`UPDATE {s}.content_version_text SET title='RESORT LOVER' WHERE tenant_id=$1 AND version_id=$2 AND language='en'`, tenant, f.vid(1, 0))
	_, page := f.list("main.test", "", q("/contents", "kind", "gallery", "language", "en"))
	if len(page.Contents) != 1 || page.Contents[0].Title != "RESORT LOVER" {
		t.Fatalf("edition title: %+v", page.Contents)
	}
	f.exec(`DELETE FROM {s}.content_text WHERE tenant_id=$1 AND content_id=$2`, tenant, f.cid(1))
	_, page = f.list("main.test", "", q("/contents", "kind", "gallery", "language", "en"))
	if len(page.Contents) != 1 || page.Contents[0].Title != "RESORT LOVER" {
		t.Fatalf("edition title incorrectly needs a parent localized title: %+v", page.Contents)
	}
	f.exec(`DELETE FROM {s}.content_version_text WHERE tenant_id=$1 AND version_id=$2`, tenant, f.vid(1, 0))
	_, page = f.list("main.test", "", q("/contents", "kind", "gallery", "language", "en"))
	if len(page.Contents) != 1 || page.Contents[0].Title != "原題" {
		t.Fatalf("original fallback: %+v", page.Contents)
	}
	f.exec(`INSERT INTO {s}.content_text (tenant_id,content_id,language,title,slug) VALUES ($1,$2,'en','Legacy English','legacy-english')`, tenant, f.cid(1))
	f.exec(`UPDATE {s}.content_items SET original_title=NULL WHERE tenant_id=$1 AND id=$2`, tenant, f.cid(1))
	_, page = f.list("main.test", "", q("/contents", "kind", "gallery", "language", "en"))
	if len(page.Contents) != 1 || page.Contents[0].Title != "Legacy English" {
		t.Fatalf("legacy fallback: %+v", page.Contents)
	}
}
