package contentkit

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/signal"
)

// fixedCandidates is a Candidates source returning fixed lists (or err).
type fixedCandidates struct {
	similar, forSubject []discovery.Candidate
	err                 error
	queries             *[]discovery.Query
}

func (f fixedCandidates) Similar(_ context.Context, _ ContentRef, q discovery.Query) ([]discovery.Candidate, error) {
	return f.answer(q, f.similar)
}

func (f fixedCandidates) ForSubject(_ context.Context, _ signal.Subject, q discovery.Query) ([]discovery.Candidate, error) {
	return f.answer(q, f.forSubject)
}

func (f fixedCandidates) answer(q discovery.Query, out []discovery.Candidate) ([]discovery.Candidate, error) {
	if f.queries != nil {
		*f.queries = append(*f.queries, q)
	}
	return out, f.err
}

func cands(refs ...ContentRef) []discovery.Candidate {
	out := make([]discovery.Candidate, 0, len(refs))
	for i, ref := range refs {
		out = append(out, discovery.Candidate{Ref: ref, Score: float64(100 - i)})
	}
	return out
}

func hitIDs(hits []RecHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ContentID)
	}
	return out
}

func TestHubCustomCandidatesGetSharedPolicy(t *testing.T) {
	ctx := context.Background()
	var queries []discovery.Query
	custom := fixedCandidates{queries: &queries}
	list := cands(
		gallery("anchor"), gallery("hated"), gallery("seen1"),
		contentref.New(testTenant, "video", "v1"), contentref.New("hentai0", "gallery", "foreign"),
		gallery("good1"), gallery("good1"), gallery("good2"),
	)
	custom.similar, custom.forSubject = list, list[1:]
	h := signalHub(t, func(cfg *EmbeddedConfig) { cfg.Candidates = custom })

	u1 := signal.Subject{UserID: "u1"}
	batch := []signal.Signal{
		hubView(testTenant, "seen1", "u1", 1, 50),
		{ContentRef: gallery("hated"), Subject: u1, Type: "reaction", EventID: "dislike", OccurredAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC), Value: -1},
	}
	for i := 1; i <= 3; i++ {
		batch = append(batch, hubView(testTenant, "pop1", fmt.Sprintf("p%d", i), 1, 50))
		if i <= 2 {
			batch = append(batch, hubView(testTenant, "pop2", fmt.Sprintf("p%d", i), 1, 50))
		}
	}
	if err := h.RecordSignals(ctx, batch); err != nil {
		t.Fatal(err)
	}

	sim, err := h.SimilarTo(ctx, gallery("anchor"), SimilarOptions{ContentKinds: []string{"gallery"}, ExcludeSeenFor: &u1})
	if err != nil {
		t.Fatal(err)
	}
	if got := hitIDs(sim); !reflect.DeepEqual(got, []string{"good1", "good2"}) {
		t.Fatalf("similar must drop anchor, disliked, seen, other kinds, other tenants and duplicates: %v", got)
	}
	sim, err = h.SimilarTo(ctx, gallery("anchor"), SimilarOptions{ContentKinds: []string{"gallery"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := hitIDs(sim); !reflect.DeepEqual(got, []string{"hated", "seen1", "good1", "good2"}) {
		t.Fatalf("similar without a subject keeps seen and disliked: %v", got)
	}
	if _, err := h.SimilarTo(ctx, contentref.New("hentai0", "gallery", "x"), SimilarOptions{}); err == nil {
		t.Fatal("foreign tenant anchor accepted")
	}

	recs, err := h.Recommend(ctx, u1, RecommendOptions{ContentKinds: []string{"gallery"}, Limit: 4, PopularWindow: signal.AllTime()})
	if err != nil {
		t.Fatal(err)
	}
	if got := hitIDs(recs); !reflect.DeepEqual(got, []string{"good1", "good2", "pop1", "pop2"}) {
		t.Fatalf("recommend must filter candidates and fill from popularity: %v", got)
	}
	if recs[2].Score >= recs[1].Score || recs[3].Score >= recs[2].Score {
		t.Fatalf("popularity fill must rank after candidates: %+v", recs)
	}
	last := queries[len(queries)-1]
	if !reflect.DeepEqual(last.ContentKinds, []string{"gallery"}) || last.Limit != 4 {
		t.Fatalf("query: %+v", last)
	}
}

func TestHubFallbackCandidatesOverEngagement(t *testing.T) {
	ctx := context.Background()
	primary := &fixedCandidates{}
	var reported []error
	conn := signaltest.FromEnv(t).Fresh(t, hubSignalTestCHDB)
	engagement, err := discovery.NewEngagement(conn, hubSignalTestCHDB, testTenant)
	if err != nil {
		t.Fatal(err)
	}
	h := newTestHub(t, conn, hubSignalTestCHDB, func(cfg *EmbeddedConfig) {
		cfg.Candidates = discovery.Fallback{
			Primary:   primary,
			Secondary: engagement,
			OnError:   func(err error) { reported = append(reported, err) },
		}
	})

	var batch []signal.Signal
	for _, fan := range []string{"f1", "f2"} {
		batch = append(batch, hubView(testTenant, "seed1", fan, 1, 60), hubView(testTenant, "gA", fan, 1, 60), hubView(testTenant, "gB", fan, 1, 60))
	}
	batch = append(batch, hubView(testTenant, "seed1", "u1", 2, 90))
	if err := h.RecordSignals(ctx, batch); err != nil {
		t.Fatal(err)
	}
	u1 := signal.Subject{UserID: "u1"}
	opts := RecommendOptions{ContentKinds: []string{"gallery"}, Limit: 3}

	primary.err = errors.New("model offline")
	recs, err := h.Recommend(ctx, u1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := hitIDs(recs); !reflect.DeepEqual(got, []string{"gA", "gB"}) || len(reported) != 1 {
		t.Fatalf("primary error must fall back to engagement: %v reported=%v", got, reported)
	}
	sim, err := h.SimilarTo(ctx, gallery("seed1"), SimilarOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := hitIDs(sim); !reflect.DeepEqual(got, []string{"gA", "gB"}) || len(reported) != 2 {
		t.Fatalf("similar fallback: %v reported=%v", got, reported)
	}

	primary.err = nil
	primary.forSubject = cands(gallery("gB"), gallery("gX"))
	recs, err = h.Recommend(ctx, u1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := hitIDs(recs); !reflect.DeepEqual(got, []string{"gB", "gX", "gA"}) || len(reported) != 2 {
		t.Fatalf("thin primary must be filled from engagement, deduplicated: %v", got)
	}
}
