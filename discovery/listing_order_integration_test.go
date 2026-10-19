package discovery

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// Contents with several versions page once each, in every sort's order, by
// cursor and by number, with the display language's version.
func TestPagesListEachContentOnceAcrossVersions(t *testing.T) {
	f := newFixture(t, nil, "")
	items := make([]item, 0, 6)
	for n := 1; n <= 6; n++ {
		items = append(items, item{n: n, titles: map[string]string{"en": fmt.Sprintf("Order %d", 7-n)}, age: time.Duration(n) * time.Hour,
			versions: []ver{{langs: []string{"ja"}}, {langs: []string{"en"}}}})
	}
	f.seed(items...)
	const host = "main.test"
	byCursor := func(sort string) []string {
		var got []string
		for cursor := ""; ; {
			kv := []string{"kind", "gallery", "sort", sort, "limit", "2"}
			if cursor != "" {
				kv = append(kv, "cursor", cursor)
			}
			ids, page := f.list(host, "", q("/contents", kv...))
			for _, c := range page.Contents {
				if !reflect.DeepEqual(c.Version.Languages, []string{"en"}) {
					t.Fatalf("%s: card %s shows version %v, want the English one", sort, c.ID, c.Version.Languages)
				}
			}
			got = append(got, ids...)
			if cursor = page.NextCursor; cursor == "" {
				return got
			}
		}
	}
	newest := []string{f.cid(1), f.cid(2), f.cid(3), f.cid(4), f.cid(5), f.cid(6)}
	if got := byCursor("newest"); !reflect.DeepEqual(got, newest) {
		t.Fatalf("newest by cursor = %v, want %v", got, newest)
	}
	byTitle := []string{f.cid(6), f.cid(5), f.cid(4), f.cid(3), f.cid(2), f.cid(1)}
	if got := byCursor("title"); !reflect.DeepEqual(got, byTitle) {
		t.Fatalf("title by cursor = %v, want %v", got, byTitle)
	}
	if got, _ := f.list(host, "", q("/contents", "kind", "gallery", "sort", "newest", "limit", "2", "page", "2")); !reflect.DeepEqual(got, newest[2:4]) {
		t.Fatalf("newest page 2 = %v, want %v", got, newest[2:4])
	}
}

// Copied counts can differ between a content's versions until the counts job
// runs; paging by them still lists each content once.
func TestPagesByCopiedCountsListEachContentOnce(t *testing.T) {
	f := newFixture(t, nil, "")
	for n := 1; n <= 3; n++ {
		f.seed(item{n: n, titles: map[string]string{"en": fmt.Sprintf("Counted %d", n)}, age: time.Duration(n) * time.Hour,
			versions: []ver{{langs: []string{"en"}}, {langs: []string{"ja"}}}})
	}
	f.exec(`UPDATE `+f.table("content_facets")+` SET favorites = 5, rating = 0.5 WHERE version_id = $1`, f.vid(1, 0))
	for _, sort := range []string{"most_favorited", "top_rated"} {
		var got []string
		for cursor := ""; ; {
			kv := []string{"kind", "gallery", "sort", sort, "limit", "1"}
			if cursor != "" {
				kv = append(kv, "cursor", cursor)
			}
			ids, page := f.list("main.test", "", q("/contents", kv...))
			got = append(got, ids...)
			if cursor = page.NextCursor; cursor == "" {
				break
			}
		}
		if len(got) != 3 || got[0] != f.cid(1) {
			t.Fatalf("%s by cursor = %v, want content 1 first and each content once", sort, got)
		}
		var numbered []string
		for _, page := range []string{"1", "2", "3", "4"} {
			ids, _ := f.list("main.test", "", q("/contents", "kind", "gallery", "sort", sort, "limit", "1", "page", page))
			numbered = append(numbered, ids...)
		}
		if !reflect.DeepEqual(numbered, got) {
			t.Fatalf("%s numbered pages = %v, want %v", sort, numbered, got)
		}
	}
}
