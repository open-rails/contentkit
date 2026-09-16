package searchkit

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/searchkit/signal"
)

func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	// pgxpool connects lazily; these unit tests never touch Postgres.
	pool, err := pgxpool.New(context.Background(), "postgres://unused:unused@127.0.0.1:9/unused")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newTestHub(t *testing.T, ch signal.Conn, database string, mutate func(*EmbeddedConfig)) *EmbeddedHub {
	t.Helper()
	cfg := EmbeddedConfig{
		PG:       lazyPool(t),
		PGSchema: "hub",
		Tenant:   "doujins",
	}
	if ch != nil {
		cfg.CH = ch
		cfg.CHDatabase = database
	}
	if mutate != nil {
		mutate(&cfg)
	}
	h, err := NewEmbedded(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// --- tests ---

func TestHubSignalPlaneDisabled(t *testing.T) {
	h := newTestHub(t, nil, "", nil)
	ctx := context.Background()
	sub := signal.Subject{UserID: "u1"}

	if err := h.RecordSignals(ctx, []signal.Signal{{}}); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("RecordSignals: %v", err)
	}
	if _, err := h.History(ctx, sub, signal.HistoryOptions{}); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("History: %v", err)
	}
	if _, err := h.States(ctx, sub, nil); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("States: %v", err)
	}
	if _, err := h.Metrics(ctx, "gallery", []string{"1"}, signal.AllTime()); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("Metrics: %v", err)
	}
	if _, err := h.RepairProjections(ctx, signal.RepairOptions{}); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("RepairProjections: %v", err)
	}
	if _, err := h.EraseSubjects(ctx, []signal.Subject{sub}); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("EraseSubjects: %v", err)
	}
	if _, err := h.Popular(ctx, "gallery", signal.PopularOptions{}); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("Popular: %v", err)
	}
	if _, err := h.Unseen(ctx, sub, UnseenOptions{EntityType: "gallery"}); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("Unseen: %v", err)
	}
	if _, err := h.Recommend(ctx, sub, RecommendOptions{EntityTypes: []string{"gallery"}}); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("Recommend: %v", err)
	}
	if _, err := h.Search(ctx, "query", HubSearchOptions{Personalize: &Personalization{Subject: sub}}); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("personalized Search: %v", err)
	}
}

func TestHubDefaultTenant(t *testing.T) {
	h := newTestHub(t, nil, "", func(c *EmbeddedConfig) { c.Tenant = "" })
	if h.Tenant() != "default" {
		t.Fatalf("tenant: %q", h.Tenant())
	}
}
