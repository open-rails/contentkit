package search

import (
	"context"
	"math"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
)

func key(id string) RRFKey {
	return RRFKey{ContentKey: contentref.ContentKey{TenantID: "t", ContentKind: "gallery", ContentID: id}, Language: "en"}
}

func TestMergeNamedArgs(t *testing.T) {
	t.Parallel()
	if err := mergeNamedArgs(pgx.NamedArgs{"tenant": "x"}, map[string]any{"tenant": "y"}); err == nil {
		t.Fatal("expected conflict error")
	}
	if err := mergeNamedArgs(pgx.NamedArgs{}, map[string]any{"": 1}); err == nil {
		t.Fatal("expected empty key error")
	}
	for _, name := range []string{"tenant", "language", "q", "prefix", "limit", "kinds", "candidates"} {
		args := pgx.NamedArgs{"q": "", "prefix": "", "limit": 0, "candidates": nil}
		_, _, _, err := hostClauses(Options{Schema: "s", Tenant: "t", Language: "en", ContentKinds: []string{"k"}, FilterSQL: "1=1", FilterArgs: map[string]any{name: 2}}, args)
		if err == nil {
			t.Fatalf("reserved arg %s accepted", name)
		}
	}
	if _, _, _, err := hostClauses(Options{Schema: "s", Language: "en"}, pgx.NamedArgs{}); err == nil {
		t.Fatal("tenant is required")
	}
}

func TestKeywordSearchValidation(t *testing.T) {
	t.Parallel()
	if _, err := KeywordSearch(context.Background(), nil, "x", Options{Schema: "s", Tenant: "t", Language: "en", Limit: 1}); err == nil {
		t.Fatal("nil pool must error")
	}
	if _, err := Eligible(context.Background(), nil, Options{}, []Candidate{{}}); err == nil {
		t.Fatal("nil pool must error")
	}
}

func TestFuseRRFWithTrace_MatchesFuseRRF(t *testing.T) {
	t.Parallel()
	lists := [][]RRFKey{{key("1"), key("2")}, {key("2"), key("3")}}
	opts := RRFOptions{K: 10, Weights: []float32{1, 2}}
	want := FuseRRF(lists, opts)
	traced, err := FuseRRFWithTrace(lists, opts)
	if err != nil {
		t.Fatalf("FuseRRFWithTrace() error = %v", err)
	}
	got := make([]RRFHit, 0, len(traced))
	for _, hit := range traced {
		got = append(got, hit.Hit)
		var sum float32
		for _, contribution := range hit.Contributions {
			sum += contribution.Contribution
		}
		if sum != hit.Hit.Score {
			t.Fatalf("contributions sum = %v, score = %v", sum, hit.Hit.Score)
		}
	}
	if !reflect.DeepEqual(got, want) || want[0].ContentID != "2" {
		t.Fatalf("traced hits differ:\n got %#v\nwant %#v", got, want)
	}
	if len(traced[0].Contributions) != 2 || traced[0].Contributions[0].ListIndex != 0 || traced[0].Contributions[1].ListIndex != 1 {
		t.Fatalf("unexpected contributions: %#v", traced[0].Contributions)
	}
}

func TestFuseRRFWithTrace_RejectsNonfiniteAndOverflowingScores(t *testing.T) {
	t.Parallel()
	lists := [][]RRFKey{{key("1")}}
	for _, weight := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		if _, err := FuseRRFWithTrace(lists, RRFOptions{Weights: []float32{weight}}); err == nil {
			t.Fatalf("FuseRRFWithTrace(weight=%v) error = nil", weight)
		}
	}
	if _, err := FuseRRFWithTrace([][]RRFKey{lists[0], lists[0], lists[0]}, RRFOptions{K: 1, Weights: []float32{math.MaxFloat32, math.MaxFloat32, math.MaxFloat32}}); err == nil {
		t.Fatal("FuseRRFWithTrace() error = nil, want score overflow")
	}
}

func TestFuseRRF_DeterministicFullKeyTieBreak(t *testing.T) {
	t.Parallel()
	ja, en, v := key("1"), key("1"), key("1")
	ja.Language = "ja"
	v.ContentVersionID = "v2"
	lists := [][]RRFKey{{ja}, {v}, {en}}
	// Identity (version) orders before metadata (language).
	want := []RRFKey{en, ja, v}
	for range 20 {
		got := FuseRRF(lists, RRFOptions{K: 60})
		for i := range want {
			if got[i].RRFKey != want[i] {
				t.Fatalf("FuseRRF()[%d] = %#v, want %#v", i, got[i].RRFKey, want[i])
			}
		}
	}
}

func TestFuseRRF_LargeKDoesNotOverflow(t *testing.T) {
	t.Parallel()
	out := FuseRRF([][]RRFKey{{key("1")}}, RRFOptions{K: int(^uint(0) >> 1)})
	if len(out) != 1 || out[0].Score < 0 || !finiteFloat32(out[0].Score) {
		t.Fatalf("FuseRRF() produced invalid large-k score: %#v", out)
	}
}
