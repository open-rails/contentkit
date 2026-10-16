package discovery

import (
	"errors"
	"testing"
	"time"
)

func TestHistoricalActivityWindowValidation(t *testing.T) {
	base := ListOptions{Filter: Filter{Kind: "gallery"}, Sort: SortPopular, ActivityFrom: "2012-01-01", ActivityUntil: "2026-01-01"}
	w, custom, err := activityWindow(base)
	if err != nil || !custom || !w.From.Equal(time.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC)) || !w.To.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("bounded UTC range: %+v %v %v", w, custom, err)
	}
	for _, tc := range []struct {
		name string
		edit func(*ListOptions)
	}{
		{"no from", func(o *ListOptions) { o.ActivityFrom = "" }},
		{"no until", func(o *ListOptions) { o.ActivityUntil = "" }},
		{"no kind", func(o *ListOptions) { o.Kind = "" }},
		{"no sort", func(o *ListOptions) { o.Sort = "" }},
		{"newest sort", func(o *ListOptions) { o.Sort = SortNewest }},
		{"preset sort", func(o *ListOptions) { o.Sort = "popular:7d" }},
		{"malformed date", func(o *ListOptions) { o.ActivityFrom = "2012-1-01" }},
		{"timestamp", func(o *ListOptions) { o.ActivityFrom = "2012-01-01T00:00:00Z" }},
		{"invalid day", func(o *ListOptions) { o.ActivityUntil = "2026-02-30" }},
		{"unbounded zero time", func(o *ListOptions) { o.ActivityFrom = "0001-01-01" }},
		{"equal dates", func(o *ListOptions) { o.ActivityUntil = o.ActivityFrom }},
		{"backwards", func(o *ListOptions) { o.ActivityUntil = "2011-01-01" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := base
			tc.edit(&o)
			if _, _, err := activityWindow(o); !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v, want invalid request", err)
			}
		})
	}
	if _, custom, err := activityWindow(ListOptions{}); err != nil || custom {
		t.Fatalf("ordinary list changed: %v %v", custom, err)
	}
}
