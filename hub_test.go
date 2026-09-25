package contentkit

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/signal"
)

func TestHubSignalPlaneDisabled(t *testing.T) {
	h := newTestHub(t, nil, "", nil)
	ctx := context.Background()
	sub := signal.Subject{UserID: "u1"}
	g1 := h.Content("gallery", cid(1))

	if err := h.RecordSignals(ctx, []signal.Signal{{}}); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("RecordSignals: %v", err)
	}
	if _, err := h.History(ctx, sub, signal.HistoryOptions{}); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("History: %v", err)
	}
	if _, err := h.States(ctx, sub, nil); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("States: %v", err)
	}
	if _, err := h.Metrics(ctx, []ContentRef{g1}, signal.AllTime()); !errors.Is(err, ErrSignalPlaneDisabled) {
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
	if _, err := h.Unseen(ctx, sub, UnseenOptions{ContentKind: "gallery"}); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("Unseen: %v", err)
	}
	if _, err := h.Recommend(ctx, sub, RecommendOptions{ContentKinds: []string{"gallery"}}); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("Recommend: %v", err)
	}
	if _, err := h.SimilarTo(ctx, g1, SimilarOptions{}); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("SimilarTo: %v", err)
	}
	if _, err := h.Search(ctx, "query", HubSearchOptions{Personalize: &Personalization{Subject: sub}}); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("personalized Search: %v", err)
	}
	if err := h.PurgeContentKinds(ctx, []string{"gallery"}); !errors.Is(err, ErrSignalPlaneDisabled) {
		t.Fatalf("PurgeContentKinds: %v", err)
	}
}

func TestHubRequiresTenant(t *testing.T) {
	_, err := NewEmbedded(EmbeddedConfig{PG: lazyPool(t), PGSchema: "hub"})
	if err == nil || !strings.Contains(err.Error(), "Tenant") {
		t.Fatalf("NewEmbedded without a tenant: %v", err)
	}
	h := newTestHub(t, nil, "", nil)
	if h.Tenant() != testTenant || h.Content("gallery", cid(1)).TenantID != testTenant {
		t.Fatalf("tenant: %q", h.Tenant())
	}
}
