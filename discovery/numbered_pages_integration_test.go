package discovery

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestNumberedPagesCarryTheSelectionTotal(t *testing.T) {
	f := newFixture(t, nil, "")
	items := make([]item, 0, 5)
	for n := 1; n <= 5; n++ {
		items = append(items, item{n: n, titles: map[string]string{"en": fmt.Sprintf("Numbered %d", n)}, age: time.Duration(n) * time.Hour,
			versions: []ver{{langs: []string{"en"}}}})
	}
	f.seed(items...)
	const host = "main.test"
	newest := func(page string) ([]string, ContentPage) {
		return f.list(host, "", q("/contents", "kind", "gallery", "sort", "newest", "limit", "2", "page", page, "summary", "true"))
	}
	var got [][]string
	for _, n := range []string{"1", "2", "3", "4"} {
		ids, page := newest(n)
		if page.Summary == nil || page.Summary.Total != 5 {
			t.Fatalf("page %s summary = %+v, want total 5 on every numbered page", n, page.Summary)
		}
		got = append(got, ids)
	}
	if want := [][]string{{f.cid(1), f.cid(2)}, {f.cid(3), f.cid(4)}, {f.cid(5)}, {}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("numbered pages = %v, want %v (a page past the end is empty)", got, want)
	}
	_, cursorPage := f.list(host, "", q("/contents", "kind", "gallery", "limit", "2", "summary", "true"))
	if cursorPage.Summary == nil || cursorPage.Summary.Total != 5 || cursorPage.NextCursor == "" {
		t.Fatalf("cursor first page = %+v", cursorPage)
	}
	if _, next := f.list(host, "", q("/contents", "kind", "gallery", "limit", "2", "summary", "true", "cursor", cursorPage.NextCursor)); next.Summary != nil {
		t.Fatalf("cursor pages after the first carry no summary: %+v", next.Summary)
	}

	f.docs(doc("gallery", f.cid(1), "en", "Numbered one"), doc("gallery", f.cid(2), "en", "Numbered two"), doc("gallery", f.cid(3), "en", "Numbered three"),
		doc("gallery", f.cid(4), "en", "Numbered four"), doc("gallery", f.cid(5), "en", "Numbered five"))
	bySort, page := f.list(host, "", q("/search", "q", "numbered", "kind", "gallery", "sort", "newest", "limit", "2", "page", "2", "summary", "true"))
	if !reflect.DeepEqual(bySort, []string{f.cid(3), f.cid(4)}) || page.Summary == nil || page.Summary.Total != 5 {
		t.Fatalf("sorted search page 2 = %v %+v", bySort, page.Summary)
	}
	var relevance []string
	for _, n := range []string{"1", "2", "3"} {
		ids, page := f.list(host, "", q("/search", "q", "numbered", "kind", "gallery", "limit", "2", "page", n, "summary", "true"))
		if page.Summary == nil || page.Summary.Total != 5 {
			t.Fatalf("relevance page %s summary = %+v", n, page.Summary)
		}
		relevance = append(relevance, ids...)
	}
	if want := []string{f.cid(1), f.cid(2), f.cid(3), f.cid(4), f.cid(5)}; !slices.Equal(sortedIDs(relevance), sortedIDs(want)) {
		t.Fatalf("relevance pages = %v, want each match once", relevance)
	}

	for _, bad := range []string{
		q("/contents", "kind", "gallery", "page", "0"),
		q("/contents", "kind", "gallery", "page", "2", "cursor", cursorPage.NextCursor),
		q("/contents", "kind", "gallery", "sort", "random", "page", "1"),
		q("/contents", "kind", "gallery", "limit", "100", "page", "101"),
		q("/search", "q", "numbered", "page", "-1"),
	} {
		if status, _ := f.call(http.MethodGet, host, "", bad, nil, nil); status != http.StatusBadRequest {
			t.Errorf("GET %s: %d, want 400", bad, status)
		}
	}
}

func sortedIDs(ids []string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}
