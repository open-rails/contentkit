package contentkit

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/signal"
)

const hubSignalTestCHDB = "contentkit_hub_signal_test"

// signalHub is an embedded hub over a real ClickHouse with the migrated signal
// schema and no content model.
func signalHub(t *testing.T, mutate func(*EmbeddedConfig)) *EmbeddedHub {
	t.Helper()
	hub, _ := signalHubConn(t, mutate)
	return hub
}

func signalHubConn(t *testing.T, mutate func(*EmbeddedConfig)) (*EmbeddedHub, signal.Conn) {
	t.Helper()
	env := signaltest.FromEnv(t)
	conn := env.Fresh(t, hubSignalTestCHDB)
	return newTestHub(t, conn, hubSignalTestCHDB, mutate), conn
}

func hubView(tenant, id, subject string, day int, score int16) signal.Signal {
	return signal.Signal{
		ContentRef: contentref.New(tenant, "gallery", id),
		Subject:    signal.Subject{UserID: subject},
		Type:       signal.TypeView,
		EventID:    id + ":" + subject,
		OccurredAt: time.Date(2026, 6, day, 10, 0, 0, 0, time.UTC),
		Progress:   10, ProgressMax: 10, Score: score,
	}
}

func TestHubRecordSignalsAppliesScorersAtomically(t *testing.T) {
	calls := 0
	h := signalHub(t, func(c *EmbeddedConfig) {
		c.Scorers = map[string]signal.Scorer{
			"blog_post": signal.ScorerFunc(func(_ context.Context, s signal.Signal) (signal.Scored, error) {
				calls++
				if s.ContentID == cid(2) {
					return signal.Scored{}, fmt.Errorf("boom")
				}
				return signal.Scored{Score: 77, Progress: 95, ProgressMax: 100, Completed: true}, nil
			}),
		}
	})
	ctx := context.Background()
	user := signal.Subject{UserID: "u1"}
	post := func(id string) signal.Signal {
		return signal.Signal{ContentRef: h.Content("blog_post", id), Subject: user,
			Type: signal.TypeView, EventID: "read:" + id, OccurredAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC), Progress: 1}
	}
	err := h.RecordSignals(ctx, []signal.Signal{post(cid(1)), post(cid(2))})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("scorer error must propagate: %v", err)
	}
	refs := []ContentRef{h.Content("blog_post", cid(1))}
	if states, err := h.States(ctx, user, refs); err != nil || len(states) != 0 {
		t.Fatalf("a failed scorer must record nothing from the batch: %v %v", states, err)
	}
	if err := h.RecordSignals(ctx, []signal.Signal{post(cid(1))}); err != nil {
		t.Fatal(err)
	}
	states, err := h.States(ctx, user, refs)
	if err != nil {
		t.Fatal(err)
	}
	if s := states[refs[0].Key()]; s.LastScore != 77 || s.MaxProgress != 95 || s.ProgressMax != 100 || !s.Completed {
		t.Fatalf("scorer result not recorded: %+v", s)
	}
	if calls != 3 {
		t.Fatalf("scorer calls: %d", calls)
	}
	// A reference of another tenant is refused before anything is written.
	foreign := post(cid(1))
	foreign.ContentRef = contentref.New("hentai0", "blog_post", cid(1))
	if err := h.RecordSignals(ctx, []signal.Signal{foreign}); err == nil || !strings.Contains(err.Error(), "tenant") {
		t.Fatalf("foreign tenant signal accepted: %v", err)
	}
	if _, err := h.States(ctx, user, []ContentRef{foreign.ContentRef}); err == nil {
		t.Fatal("foreign tenant reference accepted by States")
	}
	if _, err := h.Metrics(ctx, []ContentRef{foreign.ContentRef}, signal.AllTime()); err == nil {
		t.Fatal("foreign tenant reference accepted by Metrics")
	}
}

func TestHubUnseenDiffsUniverseAgainstSeen(t *testing.T) {
	universeCalls := 0
	h := signalHub(t, func(c *EmbeddedConfig) {
		c.Catalogs = map[string]ContentCatalog{
			"gallery": ContentCatalogFunc(func(_ context.Context, tenant, contentKind string, q CatalogQuery) ([]string, error) {
				universeCalls++
				if tenant != testTenant || contentKind != "gallery" {
					return nil, fmt.Errorf("unexpected catalog call %s/%s", tenant, contentKind)
				}
				return []string{cid(1), cid(2), cid(3), cid(4), cid(5)}, nil
			}),
		}
	})
	ctx := context.Background()
	zero := hubView(testTenant, cid(3), "u1", 1, 0)
	zero.Progress = 0
	if err := h.RecordSignals(ctx, []signal.Signal{hubView(testTenant, cid(2), "u1", 1, 10), hubView(testTenant, cid(4), "u1", 1, 10), zero}); err != nil {
		t.Fatal(err)
	}
	user := signal.Subject{UserID: "u1"}
	got, err := h.Unseen(ctx, user, UnseenOptions{ContentKind: "gallery"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{cid(1), cid(3), cid(5)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unseen: got %v want %v (zero progress is not seen)", got, want)
	}
	got, err = h.Unseen(ctx, user, UnseenOptions{ContentKind: "gallery", Limit: 2})
	if err != nil || !reflect.DeepEqual(got, []string{cid(1), cid(3)}) {
		t.Fatalf("unseen limited: %v %v", got, err)
	}
	if universeCalls != 2 {
		t.Fatalf("universe calls: %d", universeCalls)
	}
	if _, err := h.Unseen(ctx, user, UnseenOptions{ContentKind: "video"}); err == nil {
		t.Fatal("missing catalog must error")
	}
}

func TestHubUnseenPagesPastSeenCatalogWindow(t *testing.T) {
	const window = 1000
	ids := make([]string, window+2)
	for i := range ids {
		ids[i] = cid(i + 1)
	}
	calls := 0
	h := signalHub(t, func(c *EmbeddedConfig) {
		c.Catalogs = map[string]ContentCatalog{
			"gallery": ContentCatalogFunc(func(_ context.Context, tenant, kind string, q CatalogQuery) ([]string, error) {
				if tenant != testTenant || kind != "gallery" || q.Limit != window {
					return nil, fmt.Errorf("unexpected catalog query: %s/%s %+v", tenant, kind, q)
				}
				calls++
				switch calls {
				case 1:
					return ids[:window], nil
				case 2:
					return ids[window:], nil
				default:
					return nil, fmt.Errorf("unexpected third catalog page")
				}
			}),
		}
	})
	ctx := context.Background()
	user := signal.Subject{UserID: "u1"}
	views := make([]signal.Signal, window)
	for i, id := range ids[:window] {
		views[i] = hubView(testTenant, id, user.UserID, 1, 10)
	}
	for start := 0; start < len(views); start += signal.MaxSignalsPerBatch {
		end := min(start+signal.MaxSignalsPerBatch, len(views))
		if err := h.RecordSignals(ctx, views[start:end]); err != nil {
			t.Fatal(err)
		}
	}
	got, err := h.Unseen(ctx, user, UnseenOptions{ContentKind: "gallery", Limit: 2, CatalogLimit: window})
	if err != nil || !slices.Equal(got, ids[window:]) {
		t.Fatalf("unseen past the first catalog page: %v, %v", got, err)
	}
	if calls != 2 {
		t.Fatalf("catalog pages: %d, want 2", calls)
	}
}

func TestHubRecommendFromCanonicalSignals(t *testing.T) {
	h := signalHub(t, nil)
	ctx := context.Background()
	if _, err := h.Recommend(ctx, signal.Subject{UserID: "u"}, RecommendOptions{}); err == nil {
		t.Fatal("ContentKinds must be required")
	}
	var batch []signal.Signal
	// Popularity: g1 by 6 subjects, g2 by 5 (more than any co-engaged item).
	for i := 1; i <= 6; i++ {
		batch = append(batch, hubView(testTenant, cid(1), fmt.Sprintf("p%d", i), 1, 50))
		if i <= 5 {
			batch = append(batch, hubView(testTenant, cid(2), fmt.Sprintf("p%d", i), 1, 50))
		}
	}
	// Co-engagement: seed1 fans also read gShared and gOne; seed2 fans read gShared and gTwo.
	for _, s := range []string{"f1", "f2"} {
		batch = append(batch, hubView(testTenant, cid(11), s, 2, 60), hubView(testTenant, cid(21), s, 2, 60), hubView(testTenant, cid(22), s, 2, 60))
		batch = append(batch, hubView(testTenant, cid(12), s+"b", 2, 60), hubView(testTenant, cid(21), s+"b", 2, 60), hubView(testTenant, cid(23), s+"b", 2, 60))
	}
	// u1 strongly engaged both seeds, has seen gTwo, and dislikes gOne.
	batch = append(batch, hubView(testTenant, cid(11), "u1", 3, 90), hubView(testTenant, cid(12), "u1", 3, 80), hubView(testTenant, cid(23), "u1", 3, 10),
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
	if len(sim) < 2 || sim[0].ContentID != cid(21) && sim[0].ContentID != cid(22) || slices.ContainsFunc(sim, func(r RecHit) bool { return r.ContentID == cid(11) }) {
		t.Fatalf("co-engagement similar: %+v", sim)
	}
	excluded, err := h.SimilarTo(ctx, h.Content("gallery", cid(11)), SimilarOptions{ExcludeSeenFor: &signal.Subject{UserID: "u1"}})
	if err != nil || slices.ContainsFunc(excluded, func(r RecHit) bool { return r.ContentID == cid(22) }) {
		t.Fatalf("disliked work must be excluded: %+v %v", excluded, err)
	}
	if _, err := h.SimilarTo(ctx, contentref.New("hentai0", "gallery", cid(11)), SimilarOptions{}); err == nil {
		t.Fatal("foreign tenant anchor accepted")
	}
}

func TestHubEraseSubjectsIsTenantScoped(t *testing.T) {
	h, conn := signalHubConn(t, nil)
	ctx := context.Background()
	other := newTestHub(t, conn, hubSignalTestCHDB, func(c *EmbeddedConfig) { c.Tenant = "hentai0" })
	for _, hub := range []*EmbeddedHub{h, other} {
		if err := hub.RecordSignals(ctx, []signal.Signal{hubView(hub.Tenant(), cid(1), "gone", 1, 50), hubView(hub.Tenant(), cid(1), "kept", 1, 50)}); err != nil {
			t.Fatal(err)
		}
	}
	report, err := h.EraseSubjects(ctx, []signal.Subject{{UserID: "gone"}})
	if err != nil || !report.Complete() {
		t.Fatalf("erase: %+v %v", report, err)
	}
	for _, tc := range []struct {
		hub  *EmbeddedHub
		want int
	}{{h, 0}, {other, 1}} {
		hist, err := tc.hub.History(ctx, signal.Subject{UserID: "gone"}, signal.HistoryOptions{})
		if err != nil || len(hist) != tc.want {
			t.Fatalf("%s history after erasure: %v %v", tc.hub.Tenant(), hist, err)
		}
	}
	if _, err := h.EnforceErasures(ctx); err != nil {
		t.Fatal(err)
	}
	g1 := h.Content("gallery", cid(1))
	m, err := h.Metrics(ctx, []ContentRef{g1}, signal.AllTime())
	if err != nil || m[g1.Key()].Viewers != 1 {
		t.Fatalf("metrics after erasure: %+v %v", m, err)
	}
}
