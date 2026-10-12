package discovery

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/signal"
)

const (
	recTenant = "doujins"
	recCHDB   = "contentkit_discovery_rec_test"
)

func gallery(id string) contentref.ContentRef { return contentref.New(recTenant, "gallery", id) }

func recView(tenant, id, subject string, day int, score int16) signal.Signal {
	return signal.Signal{
		ContentRef: contentref.New(tenant, "gallery", id),
		Subject:    signal.Subject{UserID: subject},
		Type:       signal.TypeView,
		EventID:    id + ":" + subject,
		OccurredAt: time.Date(2026, 6, day, 10, 0, 0, 0, time.UTC),
		Progress:   10, ProgressMax: 10, Score: score,
	}
}

// recHub is a Recommender over a fresh ClickHouse signal plane.
type recHub struct {
	Recommender
}

func (h recHub) RecordSignals(ctx context.Context, batch []signal.Signal) error {
	return h.Store.RecordSignals(ctx, recTenant, batch)
}

func (h recHub) SimilarTo(ctx context.Context, anchor contentref.ContentRef, o SimilarOptions) ([]Hit, error) {
	return h.Similar(ctx, anchor, o)
}

func (h recHub) Content(kind, id string) contentref.ContentRef {
	return contentref.New(recTenant, kind, id)
}

func recHubFor(t *testing.T, c Candidates) recHub {
	t.Helper()
	return recHubOn(t, signaltest.FromEnv(t).Fresh(t, recCHDB), c)
}

func recHubOn(t *testing.T, conn signal.Conn, c Candidates) recHub {
	t.Helper()
	store, err := signal.NewStore(conn, recCHDB)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		c = Engagement{Store: store, Tenant: recTenant}
	}
	return recHub{Recommender{Candidates: c, Store: store, Tenant: recTenant}}
}

// fixedCandidates is a Candidates source returning fixed lists (or err).
type fixedCandidates struct {
	similar, forSubject []Candidate
	err                 error
	queries             *[]Query
}

func (f fixedCandidates) Similar(_ context.Context, _ contentref.ContentRef, q Query) ([]Candidate, error) {
	return f.answer(q, f.similar)
}

func (f fixedCandidates) ForSubject(_ context.Context, _ signal.Subject, q Query) ([]Candidate, error) {
	return f.answer(q, f.forSubject)
}

func (f fixedCandidates) answer(q Query, out []Candidate) ([]Candidate, error) {
	if f.queries != nil {
		*f.queries = append(*f.queries, q)
	}
	return out, f.err
}

func cands(refs ...contentref.ContentRef) []Candidate {
	out := make([]Candidate, 0, len(refs))
	for i, ref := range refs {
		out = append(out, Candidate{Ref: ref, Score: float64(100 - i)})
	}
	return out
}

func hitIDs(hits []Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ContentID)
	}
	return out
}

func TestRecommenderCustomCandidatesGetSharedPolicy(t *testing.T) {
	ctx := context.Background()
	var queries []Query
	custom := fixedCandidates{queries: &queries}
	list := cands(
		gallery(cid(1)), gallery(cid(2)), gallery(cid(3)),
		contentref.New(recTenant, "video", cid(4)), contentref.New("hentai0", "gallery", cid(5)),
		gallery(cid(6)), gallery(cid(6)), gallery(cid(7)),
	)
	custom.similar, custom.forSubject = list, list[1:]
	h := recHubFor(t, custom)

	u1 := signal.Subject{UserID: "u1"}
	batch := []signal.Signal{
		recView(recTenant, cid(3), "u1", 1, 50),
		{ContentRef: gallery(cid(2)), Subject: u1, Type: "reaction", EventID: "dislike", OccurredAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC), Value: -1},
	}
	for i := 1; i <= 3; i++ {
		batch = append(batch, recView(recTenant, cid(8), fmt.Sprintf("p%d", i), 1, 50))
		if i <= 2 {
			batch = append(batch, recView(recTenant, cid(9), fmt.Sprintf("p%d", i), 1, 50))
		}
	}
	if err := h.RecordSignals(ctx, batch); err != nil {
		t.Fatal(err)
	}

	sim, err := h.SimilarTo(ctx, gallery(cid(1)), SimilarOptions{ContentKinds: []string{"gallery"}, ExcludeSeenFor: &u1})
	if err != nil {
		t.Fatal(err)
	}
	if got := hitIDs(sim); !reflect.DeepEqual(got, []string{cid(6), cid(7)}) {
		t.Fatalf("similar must drop anchor, disliked, seen, other kinds, other tenants and duplicates: %v", got)
	}
	sim, err = h.SimilarTo(ctx, gallery(cid(1)), SimilarOptions{ContentKinds: []string{"gallery"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := hitIDs(sim); !reflect.DeepEqual(got, []string{cid(2), cid(3), cid(6), cid(7)}) {
		t.Fatalf("similar without a subject keeps seen and disliked: %v", got)
	}
	if _, err := h.SimilarTo(ctx, contentref.New("hentai0", "gallery", cid(10)), SimilarOptions{}); err == nil {
		t.Fatal("foreign tenant anchor accepted")
	}

	recs, err := h.Recommend(ctx, u1, RecommendOptions{ContentKinds: []string{"gallery"}, Limit: 4, PopularWindow: signal.AllTime()})
	if err != nil {
		t.Fatal(err)
	}
	if got := hitIDs(recs); !reflect.DeepEqual(got, []string{cid(6), cid(7), cid(8), cid(9)}) {
		t.Fatalf("recommend must filter candidates and fill from popularity: %v", got)
	}
	if recs[2].Score >= recs[1].Score || recs[3].Score > recs[2].Score {
		t.Fatalf("popularity fill must rank after candidates: %+v", recs)
	}
	last := queries[len(queries)-1]
	if !reflect.DeepEqual(last.ContentKinds, []string{"gallery"}) || last.Limit != 4 {
		t.Fatalf("query: %+v", last)
	}
}

func TestRecommenderFallbackCandidatesOverEngagement(t *testing.T) {
	ctx := context.Background()
	primary := &fixedCandidates{}
	var reported []error
	conn := signaltest.FromEnv(t).Fresh(t, recCHDB)
	engagement, err := NewEngagement(conn, recCHDB, recTenant)
	if err != nil {
		t.Fatal(err)
	}
	h := recHubOn(t, conn, Fallback{
		Primary:   primary,
		Secondary: engagement,
		OnError:   func(err error) { reported = append(reported, err) },
	})

	var batch []signal.Signal
	for _, fan := range []string{"f1", "f2"} {
		batch = append(batch, recView(recTenant, cid(1), fan, 1, 60), recView(recTenant, cid(2), fan, 1, 60), recView(recTenant, cid(3), fan, 1, 60))
	}
	batch = append(batch, recView(recTenant, cid(1), "u1", 2, 90))
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
	if got := hitIDs(recs); !reflect.DeepEqual(got, []string{cid(2), cid(3)}) || len(reported) != 1 {
		t.Fatalf("primary error must fall back to engagement: %v reported=%v", got, reported)
	}
	sim, err := h.SimilarTo(ctx, gallery(cid(1)), SimilarOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := hitIDs(sim); !reflect.DeepEqual(got, []string{cid(2), cid(3)}) || len(reported) != 2 {
		t.Fatalf("similar fallback: %v reported=%v", got, reported)
	}

	primary.err = nil
	primary.forSubject = cands(gallery(cid(3)), gallery(cid(4)))
	recs, err = h.Recommend(ctx, u1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := hitIDs(recs); !reflect.DeepEqual(got, []string{cid(3), cid(4), cid(2)}) || len(reported) != 2 {
		t.Fatalf("thin primary must be filled from engagement, deduplicated: %v", got)
	}
}

func TestRecommenderFallbackFillsAfterPolicyFiltering(t *testing.T) {
	ctx := t.Context()
	user := signal.Subject{UserID: "u1"}
	var secondaryQueries []Query
	var reported []error
	primary := &fixedCandidates{
		similar:    cands(gallery(cid(1)), gallery(cid(5))),
		forSubject: cands(gallery(cid(2)), gallery(cid(3))),
	}
	secondary := &fixedCandidates{
		similar:    cands(gallery(cid(5)), gallery(cid(3)), gallery(cid(4))),
		forSubject: cands(gallery(cid(3)), gallery(cid(5)), gallery(cid(4))),
		queries:    &secondaryQueries,
	}
	h := recHubFor(t, &Fallback{
		Primary: primary, Secondary: secondary,
		OnError: func(err error) { reported = append(reported, err) },
	})
	if err := h.RecordSignals(ctx, []signal.Signal{
		recView(recTenant, cid(2), "u1", 1, 10),
		{
			ContentRef: gallery(cid(5)), Subject: user, Type: "reaction", EventID: "dislike",
			OccurredAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC), Value: -1,
		},
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("similar", func(t *testing.T) {
		hits, err := h.SimilarTo(ctx, gallery(cid(1)), SimilarOptions{
			ContentKinds: []string{"gallery"}, ExcludeSeenFor: &user, Limit: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := hitIDs(hits), []string{cid(3), cid(4)}; !reflect.DeepEqual(got, want) {
			t.Fatalf("similar = %v, want %v", got, want)
		}
	})

	t.Run("recommend", func(t *testing.T) {
		hits, err := h.Recommend(ctx, user, RecommendOptions{
			ContentKinds: []string{"gallery"}, Limit: 2, PopularWindow: signal.AllTime(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := hitIDs(hits), []string{cid(3), cid(4)}; !reflect.DeepEqual(got, want) {
			t.Fatalf("recommend = %v, want %v", got, want)
		}
	})

	t.Run("full eligible primary", func(t *testing.T) {
		primary.similar = cands(gallery(cid(3)), gallery(cid(4)))
		primary.forSubject = cands(gallery(cid(3)), gallery(cid(4)))
		before := len(secondaryQueries)
		similar, err := h.SimilarTo(ctx, gallery(cid(1)), SimilarOptions{
			ContentKinds: []string{"gallery"}, ExcludeSeenFor: &user, Limit: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		recommended, err := h.Recommend(ctx, user, RecommendOptions{
			ContentKinds: []string{"gallery"}, Limit: 2, PopularWindow: signal.AllTime(),
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{cid(3), cid(4)}
		if !reflect.DeepEqual(hitIDs(similar), want) || !reflect.DeepEqual(hitIDs(recommended), want) {
			t.Fatalf("full primary: similar=%v recommend=%v, want %v", hitIDs(similar), hitIDs(recommended), want)
		}
		if len(secondaryQueries) != before {
			t.Fatalf("secondary called for full eligible primary: %d new calls", len(secondaryQueries)-before)
		}
	})

	t.Run("secondary seen kind", func(t *testing.T) {
		primary.similar = cands(contentref.New(recTenant, "video", cid(6)))
		secondary.similar = cands(gallery(cid(2)), gallery(cid(4)))
		hits, err := h.SimilarTo(ctx, gallery(cid(1)), SimilarOptions{
			ContentKinds: []string{"gallery"}, ExcludeSeenFor: &user, Limit: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := hitIDs(hits), []string{cid(4)}; !reflect.DeepEqual(got, want) {
			t.Fatalf("secondary seen filtering = %v, want %v", got, want)
		}
	})

	t.Run("secondary error with no eligible primary", func(t *testing.T) {
		primary.similar = cands(gallery(cid(1)), gallery(cid(5)))
		primary.forSubject = cands(gallery(cid(2)), gallery(cid(5)))
		secondary.err = errors.New("secondary offline")
		reported = nil
		similar, err := h.SimilarTo(ctx, gallery(cid(1)), SimilarOptions{
			ContentKinds: []string{"gallery"}, ExcludeSeenFor: &user, Limit: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		recommended, err := h.Recommend(ctx, user, RecommendOptions{
			ContentKinds: []string{"gallery"}, Limit: 2, PopularWindow: signal.AllTime(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(similar) != 0 || len(recommended) != 0 {
			t.Fatalf("filtered primary leaked: similar=%v recommend=%v", hitIDs(similar), hitIDs(recommended))
		}
		if len(reported) != 2 {
			t.Fatalf("secondary errors reported %d times, want 2", len(reported))
		}
	})

	t.Run("secondary error with empty primary", func(t *testing.T) {
		primary.similar = nil
		primary.forSubject = nil
		boom := errors.New("secondary offline")
		secondary.err = boom
		reported = nil
		if _, err := h.SimilarTo(ctx, gallery(cid(1)), SimilarOptions{
			ContentKinds: []string{"gallery"}, ExcludeSeenFor: &user, Limit: 2,
		}); !errors.Is(err, boom) {
			t.Fatalf("similar error = %v, want %v", err, boom)
		}
		if _, err := h.Recommend(ctx, user, RecommendOptions{
			ContentKinds: []string{"gallery"}, Limit: 2, PopularWindow: signal.AllTime(),
		}); !errors.Is(err, boom) {
			t.Fatalf("recommend error = %v, want %v", err, boom)
		}
		if len(reported) != 0 {
			t.Fatalf("reported fatal fallback errors: %v", reported)
		}
	})

	t.Run("secondary error with eligible primary", func(t *testing.T) {
		primary.similar = cands(gallery(cid(3)))
		primary.forSubject = cands(gallery(cid(3)))
		secondary.err = errors.New("secondary offline")
		reported = nil
		similar, err := h.SimilarTo(ctx, gallery(cid(1)), SimilarOptions{
			ContentKinds: []string{"gallery"}, ExcludeSeenFor: &user, Limit: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		recommended, err := h.Recommend(ctx, user, RecommendOptions{
			ContentKinds: []string{"gallery"}, Limit: 2, PopularWindow: signal.AllTime(),
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{cid(3)}
		if !reflect.DeepEqual(hitIDs(similar), want) || !reflect.DeepEqual(hitIDs(recommended), want) {
			t.Fatalf("partial primary: similar=%v recommend=%v, want %v", hitIDs(similar), hitIDs(recommended), want)
		}
		if len(reported) != 2 {
			t.Fatalf("secondary errors reported %d times, want 2", len(reported))
		}
	})

	t.Run("popularity fill survives secondary error", func(t *testing.T) {
		if err := h.RecordSignals(ctx, []signal.Signal{
			recView(recTenant, cid(6), "p1", 1, 10),
			recView(recTenant, cid(6), "p2", 1, 10),
			recView(recTenant, cid(6), "p3", 1, 10),
		}); err != nil {
			t.Fatal(err)
		}
		primary.forSubject = cands(gallery(cid(3)))
		secondary.err = errors.New("secondary offline")
		hits, err := h.Recommend(ctx, user, RecommendOptions{
			ContentKinds: []string{"gallery"}, Limit: 2, PopularWindow: signal.AllTime(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := hitIDs(hits), []string{cid(3), cid(6)}; !reflect.DeepEqual(got, want) {
			t.Fatalf("popularity fill = %v, want %v", got, want)
		}
	})
}

func TestRecommenderRecommendFromCanonicalSignals(t *testing.T) {
	h := recHubFor(t, nil)
	ctx := context.Background()
	if _, err := h.Recommend(ctx, signal.Subject{UserID: "u"}, RecommendOptions{}); err == nil {
		t.Fatal("ContentKinds must be required")
	}
	var batch []signal.Signal
	// Popularity: g1 by 6 subjects, g2 by 5 (more than any co-engaged item).
	for i := 1; i <= 6; i++ {
		batch = append(batch, recView(recTenant, cid(1), fmt.Sprintf("p%d", i), 1, 50))
		if i <= 5 {
			batch = append(batch, recView(recTenant, cid(2), fmt.Sprintf("p%d", i), 1, 50))
		}
	}
	// Co-engagement: seed1 fans also read gShared and gOne; seed2 fans read gShared and gTwo.
	for _, s := range []string{"f1", "f2"} {
		batch = append(batch, recView(recTenant, cid(11), s, 2, 60), recView(recTenant, cid(21), s, 2, 60), recView(recTenant, cid(22), s, 2, 60))
		batch = append(batch, recView(recTenant, cid(12), s+"b", 2, 60), recView(recTenant, cid(21), s+"b", 2, 60), recView(recTenant, cid(23), s+"b", 2, 60))
	}
	// u1 strongly engaged both seeds, has seen gTwo, and dislikes gOne.
	batch = append(batch, recView(recTenant, cid(11), "u1", 3, 90), recView(recTenant, cid(12), "u1", 3, 80), recView(recTenant, cid(23), "u1", 3, 10),
		signal.Signal{ContentRef: h.Content("gallery", cid(22)), Subject: signal.Subject{UserID: "u1"},
			Type: "reaction", EventID: "pref", OccurredAt: time.Date(2026, 6, 3, 11, 0, 0, 0, time.UTC), Value: -1})
	if err := h.RecordSignals(ctx, batch); err != nil {
		t.Fatal(err)
	}

	cold, err := h.Recommend(ctx, signal.Subject{UserID: "newbie"}, RecommendOptions{ContentKinds: []string{"gallery"}, Limit: 2, PopularWindow: signal.AllTime()})
	if err != nil {
		t.Fatal(err)
	}
	if len(cold) != 2 || cold[0].ContentID != cid(1) || cold[0].Score <= cold[1].Score {
		t.Fatalf("cold start must fall back to popularity: %+v", cold)
	}

	recs, err := h.Recommend(ctx, signal.Subject{UserID: "u1"}, RecommendOptions{ContentKinds: []string{"gallery"}, Limit: 5, SeedLimit: 2, PopularWindow: signal.AllTime()})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ContentID)
	}
	if len(ids) == 0 || ids[0] != cid(21) {
		t.Fatalf("gShared is on both seeds' lists and must lead: %v", ids)
	}
	for _, banned := range []string{cid(11), cid(12), cid(23), cid(22)} {
		if slices.Contains(ids, banned) {
			t.Fatalf("seen, seed or disliked %s recommended: %v", banned, ids)
		}
	}
	withSeen, err := h.Recommend(ctx, signal.Subject{UserID: "u1"}, RecommendOptions{ContentKinds: []string{"gallery"}, Limit: 10, SeedLimit: 2, IncludeSeen: true})
	if err != nil {
		t.Fatal(err)
	}
	var seenIDs []string
	for _, r := range withSeen {
		seenIDs = append(seenIDs, r.ContentID)
	}
	if !slices.Contains(seenIDs, cid(23)) || slices.Contains(seenIDs, cid(22)) {
		t.Fatalf("IncludeSeen keeps gTwo from seed2 but never the disliked gOne: %v", seenIDs)
	}

	sim, err := h.SimilarTo(ctx, h.Content("gallery", cid(11)), SimilarOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sim) < 2 || sim[0].ContentID != cid(21) && sim[0].ContentID != cid(22) || slices.ContainsFunc(sim, func(r Hit) bool { return r.ContentID == cid(11) }) {
		t.Fatalf("co-engagement similar: %+v", sim)
	}
	excluded, err := h.SimilarTo(ctx, h.Content("gallery", cid(11)), SimilarOptions{ExcludeSeenFor: &signal.Subject{UserID: "u1"}})
	if err != nil || slices.ContainsFunc(excluded, func(r Hit) bool { return r.ContentID == cid(22) }) {
		t.Fatalf("disliked work must be excluded: %+v %v", excluded, err)
	}
	if _, err := h.SimilarTo(ctx, contentref.New("hentai0", "gallery", cid(11)), SimilarOptions{}); err == nil {
		t.Fatal("foreign tenant anchor accepted")
	}
}
