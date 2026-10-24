package discovery

import (
	"net/http"
	"testing"
	"time"
)

// An explicit sort orders every match, not a bounded window of them: with more
// matches than any retrieval window, the newest match comes first, paging
// reaches every match and the summary counts them all.
func TestSearchSortOrdersEveryMatch(t *testing.T) {
	f := newFixture(t, nil, "")
	const n = 1100
	items := make([]item, n)
	for i := range items {
		items[i] = item{n: i + 1, titles: map[string]string{"en": "Needle"}, age: time.Duration(n-i) * time.Minute, versions: []ver{{langs: []string{"en"}}}}
	}
	f.seed(items...)
	for i := 1; i <= n; i++ {
		f.docs(doc("gallery", f.cid(i), "en", "Needle"))
	}
	const host = "main.test"
	var first ContentPage
	if status, code := f.call(http.MethodGet, host, "", q("/search", "q", "needle", "kind", "gallery", "sort", "newest", "summary", "true"), nil, &first); status != http.StatusOK {
		t.Fatalf("sorted search: %d %s", status, code)
	}
	if len(first.Contents) == 0 || first.Contents[0].ID != f.cid(n) {
		t.Fatalf("sorted search did not start with the newest match %s", f.cid(n))
	}
	if first.Summary == nil || first.Summary.Total != n || first.Summary.Truncated {
		t.Fatalf("summary = %+v, want an exact total of %d", first.Summary, n)
	}
	seen := map[string]bool{}
	path := q("/search", "q", "needle", "kind", "gallery", "sort", "newest", "limit", "100")
	for range n/100 + 2 {
		ids, page := f.list(host, "", path)
		for _, id := range ids {
			seen[id] = true
		}
		if page.NextCursor == "" {
			break
		}
		path = q("/search", "q", "needle", "kind", "gallery", "sort", "newest", "limit", "100", "cursor", page.NextCursor)
	}
	if len(seen) != n {
		t.Fatalf("paging reached %d matches, want %d", len(seen), n)
	}
}
