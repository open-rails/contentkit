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

// All matching versions share one current content aggregate. Version-specific
// and other-tenant totals must not affect the content order or card counts.
func TestPagesByLiveCountsListEachContentOnce(t *testing.T) {
	f := newFixture(t, nil, "")
	for n := 1; n <= 3; n++ {
		f.seed(item{n: n, titles: map[string]string{"en": fmt.Sprintf("Counted %d", n)}, age: time.Duration(n) * time.Hour,
			versions: []ver{{langs: []string{"en"}}, {langs: []string{"ja"}}}})
	}
	f.exec(`INSERT INTO {s}.content_interaction_counts (tenant_id,content_kind,content_id,likes,favorites) VALUES ($1,'gallery',$2,9,5)`, tenant, f.cid(1))
	f.exec(`INSERT INTO {s}.content_interaction_counts (tenant_id,content_kind,content_id,content_version_id,likes,favorites) VALUES ($1,'gallery',$2,$3,100,100)`, tenant, f.cid(2), f.vid(2, 0))
	f.exec(`INSERT INTO {s}.content_interaction_counts (tenant_id,content_kind,content_id,likes,favorites) VALUES ('other','gallery',$1,100,100)`, f.cid(3))
	for _, sort := range []string{"most_favorited", "top_rated"} {
		var got []string
		for cursor := ""; ; {
			kv := []string{"kind", "gallery", "sort", sort, "limit", "1"}
			if cursor != "" {
				kv = append(kv, "cursor", cursor)
			}
			ids, page := f.list("main.test", "", q("/contents", kv...))
			for _, card := range page.Contents {
				likes, favorites := 0, 0
				if card.ID == f.cid(1) {
					likes, favorites = 9, 5
				}
				if card.Counts.Likes == nil || card.Counts.Favorites == nil || *card.Counts.Likes != likes || *card.Counts.Favorites != favorites {
					t.Fatalf("%s current counts: %+v", sort, card)
				}
			}
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
