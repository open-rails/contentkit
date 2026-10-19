package catalog

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
)

func TestChapterStartInputs(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	got, err := checkSegmentInputs([]SegmentInput{{StartPage: a}, {StartPage: b, Title: "Next"}})
	if err != nil || len(got) != 2 || got[1].Position != 1 || got[1].StartPage != b || got[1].End != 0 {
		t.Fatalf("anchor-only input: %+v %v", got, err)
	}
	for _, inputs := range [][]SegmentInput{
		{{StartPage: a}, {StartPage: a}},
		{{StartPage: "001"}},
		{{StartPage: strings.ToUpper(a)}},
		{{StartPage: a, End: 10}},
		{{StartPage: a}, {Start: 2, End: 4}},
	} {
		if _, err := checkSegmentInputs(inputs); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted invalid inputs %+v: %v", inputs, err)
		}
	}
	if _, err := checkSegmentInputs([]SegmentInput{{Start: 0, End: 2}, {Start: 2, End: 4}}); err != nil {
		t.Fatalf("legacy ranges rejected: %v", err)
	}
}

func TestChapterStartStorageKeepsLegacyRanges(t *testing.T) {
	f := newFixture(t, nil)
	id, version := contentref.NewID(), contentref.NewID()
	in := ImportContent{ID: id, Kind: "gallery", State: StateDraft, Text: map[string]Text{"en": {Title: "Anchored draft"}},
		Versions: []ImportVersion{{ID: version, Languages: []string{"en"}, Segments: []Segment{{StartPage: strings.Repeat("a", 64)}, {StartPage: strings.Repeat("b", 64)}}}}}
	write := func() error {
		return pgx.BeginFunc(f.ctx, f.pool, func(tx pgx.Tx) error { return f.s.Import(f.ctx, tx, []ImportContent{in}) })
	}
	if err := write(); err != nil {
		t.Fatal(err)
	}
	if err := write(); err != nil {
		t.Fatalf("idempotent anchored import: %v", err)
	}
	var anchored int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.s.t.segments+` WHERE tenant_id=$1 AND version_id=$2 AND start_page IS NOT NULL AND start_index IS NULL AND end_index IS NULL`, tenant, version).Scan(&anchored); err != nil || anchored != 2 {
		t.Fatalf("anchored rows=%d: %v", anchored, err)
	}
	got := f.read("staff", "main.test", id)[0]
	if got == nil || len(got.Versions[0].Segments) != 2 || got.Versions[0].Segments[1].StartPage != strings.Repeat("b", 64) {
		t.Fatalf("anchor read: %+v", got)
	}
	in.Versions[0].Segments = []Segment{{Start: 0, End: 2}, {Start: 2, End: 4}}
	if err := write(); err != nil {
		t.Fatalf("explicit legacy range import: %v", err)
	}
	got = f.read("staff", "main.test", id)[0]
	if got.Versions[0].Segments[0].StartPage != "" || got.Versions[0].Segments[1].Start != 2 || got.Versions[0].Segments[1].End != 4 {
		t.Fatalf("legacy range changed: %+v", got.Versions[0].Segments)
	}
}
