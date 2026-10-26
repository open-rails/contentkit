package critical_test

import (
	"testing"
	"time"

	ck "github.com/open-rails/contentkit"
)

// A list's counts are shared by its pages for a minute: a publication changes
// them once that minute has passed, not on its own.
func TestListingSummaryRefreshesWithinAMinute(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	public := a.client("")
	total := func() int {
		t.Helper()
		summary := true
		page, err := public.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Summary: &summary})
		must(t, err)
		if page.Summary == nil {
			t.Fatal("list returned no summary")
		}
		return page.Summary.Total
	}
	publishedArticle(t, a, "Summary first")
	if got := total(); got != 1 {
		t.Fatalf("total=%d; want 1", got)
	}
	publishedArticle(t, a, "Summary second")
	if got := total(); got != 1 {
		t.Fatalf("total within a minute of a publication=%d; want the shared 1", got)
	}
	w.clock.set(w.clock.now().Add(time.Minute + time.Second))
	if got := total(); got != 2 {
		t.Fatalf("total after a minute=%d; want 2", got)
	}
}
