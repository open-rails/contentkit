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
				if s.ContentID == "boom" {
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
	err := h.RecordSignals(ctx, []signal.Signal{post("ok"), post("boom")})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("scorer error must propagate: %v", err)
	}
	refs := []ContentRef{h.Content("blog_post", "ok")}
	if states, err := h.States(ctx, user, refs); err != nil || len(states) != 0 {
		t.Fatalf("a failed scorer must record nothing from the batch: %v %v", states, err)
	}
	if err := h.RecordSignals(ctx, []signal.Signal{post("ok")}); err != nil {
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
	foreign := post("ok")
	foreign.ContentRef = contentref.New("hentai0", "blog_post", "ok")
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
				return []string{"a", "b", "c", "d", "e"}, nil
			}),
		}
	})
	ctx := context.Background()
	zero := hubView(testTenant, "c", "u1", 1, 0)
	zero.Progress = 0
	if err := h.RecordSignals(ctx, []signal.Signal{hubView(testTenant, "b", "u1", 1, 10), hubView(testTenant, "d", "u1", 1, 10), zero}); err != nil {
		t.Fatal(err)
	}
	user := signal.Subject{UserID: "u1"}
	got, err := h.Unseen(ctx, user, UnseenOptions{ContentKind: "gallery"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "c", "e"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unseen: got %v want %v (zero progress is not seen)", got, want)
	}
	got, err = h.Unseen(ctx, user, UnseenOptions{ContentKind: "gallery", Limit: 2})
	if err != nil || !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Fatalf("unseen limited: %v %v", got, err)
	}
	if universeCalls != 2 {
		t.Fatalf("universe calls: %d", universeCalls)
	}
	if _, err := h.Unseen(ctx, user, UnseenOptions{ContentKind: "video"}); err == nil {
		t.Fatal("missing catalog must error")
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
		batch = append(batch, hubView(testTenant, "g1", fmt.Sprintf("p%d", i), 1, 50))
		if i <= 5 {
			batch = append(batch, hubView(testTenant, "g2", fmt.Sprintf("p%d", i), 1, 50))
		}
	}
	// Co-engagement: seed1 fans also read gShared and gOne; seed2 fans read gShared and gTwo.
	for _, s := range []string{"f1", "f2"} {
		batch = append(batch, hubView(testTenant, "seed1", s, 2, 60), hubView(testTenant, "gShared", s, 2, 60), hubView(testTenant, "gOne", s, 2, 60))
		batch = append(batch, hubView(testTenant, "seed2", s+"b", 2, 60), hubView(testTenant, "gShared", s+"b", 2, 60), hubView(testTenant, "gTwo", s+"b", 2, 60))
	}
	// u1 strongly engaged both seeds, has seen gTwo, and dislikes gOne.
	batch = append(batch, hubView(testTenant, "seed1", "u1", 3, 90), hubView(testTenant, "seed2", "u1", 3, 80), hubView(testTenant, "gTwo", "u1", 3, 10),
		signal.Signal{ContentRef: h.Content("gallery", "gOne"), Subject: signal.Subject{UserID: "u1"},
			Type: "reaction", EventID: "pref", OccurredAt: time.Date(2026, 6, 3, 11, 0, 0, 0, time.UTC), Value: -1})
	if err := h.RecordSignals(ctx, batch); err != nil {
		t.Fatal(err)
	}

	cold, err := h.Recommend(ctx, signal.Subject{UserID: "newbie"}, RecommendOptions{ContentKinds: []string{"gallery"}, Limit: 2, PopularWindow: signal.AllTime()})
	if err != nil {
		t.Fatal(err)
	}
	if len(cold) != 2 || cold[0].ContentID != "g1" || cold[0].Score <= cold[1].Score {
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
	if len(ids) == 0 || ids[0] != "gShared" {
		t.Fatalf("gShared is on both seeds' lists and must lead: %v", ids)
	}
	for _, banned := range []string{"seed1", "seed2", "gTwo", "gOne"} {
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
	if !slices.Contains(seenIDs, "gTwo") || slices.Contains(seenIDs, "gOne") {
		t.Fatalf("IncludeSeen keeps gTwo from seed2 but never the disliked gOne: %v", seenIDs)
	}

	sim, err := h.SimilarTo(ctx, h.Content("gallery", "seed1"), SimilarOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sim) < 2 || sim[0].ContentID != "gShared" && sim[0].ContentID != "gOne" || slices.ContainsFunc(sim, func(r RecHit) bool { return r.ContentID == "seed1" }) {
		t.Fatalf("co-engagement similar: %+v", sim)
	}
	excluded, err := h.SimilarTo(ctx, h.Content("gallery", "seed1"), SimilarOptions{ExcludeSeenFor: &signal.Subject{UserID: "u1"}})
	if err != nil || slices.ContainsFunc(excluded, func(r RecHit) bool { return r.ContentID == "gOne" }) {
		t.Fatalf("disliked work must be excluded: %+v %v", excluded, err)
	}
	if _, err := h.SimilarTo(ctx, contentref.New("hentai0", "gallery", "seed1"), SimilarOptions{}); err == nil {
		t.Fatal("foreign tenant anchor accepted")
	}
}

func TestHubEraseSubjectsIsTenantScoped(t *testing.T) {
	h, conn := signalHubConn(t, nil)
	ctx := context.Background()
	other := newTestHub(t, conn, hubSignalTestCHDB, func(c *EmbeddedConfig) { c.Tenant = "hentai0" })
	for _, hub := range []*EmbeddedHub{h, other} {
		if err := hub.RecordSignals(ctx, []signal.Signal{hubView(hub.Tenant(), "g1", "gone", 1, 50), hubView(hub.Tenant(), "g1", "kept", 1, 50)}); err != nil {
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
	g1 := h.Content("gallery", "g1")
	m, err := h.Metrics(ctx, []ContentRef{g1}, signal.AllTime())
	if err != nil || m[g1.Key()].Viewers != 1 {
		t.Fatalf("metrics after erasure: %+v %v", m, err)
	}
}
