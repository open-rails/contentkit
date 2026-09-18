package popularity

import (
	"context"
	"testing"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
)

type reviewCacheSource struct {
	tenant string
	id     string
	calls  int
}

func (s *reviewCacheSource) Tenant() string { return s.tenant }
func (s *reviewCacheSource) Popular(_ context.Context, kind string, _ signal.PopularOptions) ([]signal.PopularHit, error) {
	s.calls++
	return []signal.PopularHit{{ContentRef: contentref.New(s.tenant, kind, s.id)}}, nil
}
func (s *reviewCacheSource) Metrics(context.Context, []signal.ContentRef, signal.Window) (map[signal.ContentKey]signal.ContentMetrics, error) {
	return nil, nil
}

func TestReviewCacheSeparatesOpaqueTenantsAndPolicies(t *testing.T) {
	cache := NewMemoryCache()
	a, b := &reviewCacheSource{tenant: "site:variant", id: "private-a"}, &reviewCacheSource{tenant: "site", id: "private-b"}
	pa, pb := PolicyV1, PolicyV1
	pa.Name, pb.Name = "v1", "variant:v1"
	for _, cfg := range []Config{{Source: a, Policy: pa, Cache: cache}, {Source: b, Policy: pb, Cache: cache}} {
		r, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.Popular(context.Background(), "gallery", signal.AllTime(), 10)
		if err != nil {
			t.Fatal(err)
		}
		want := cfg.Source.(*reviewCacheSource).id
		if len(got) != 1 || got[0].ContentID != want {
			t.Fatalf("tenant %q received another tenant's cached results: %+v", cfg.Source.Tenant(), got)
		}
	}
}

func TestReviewCacheSeparatesPolicyParameters(t *testing.T) {
	cache := NewMemoryCache()
	source := &reviewCacheSource{tenant: "site", id: "a"}
	changed := PolicyV1
	changed.Weights.Approval = 0.8
	for _, policy := range []Policy{PolicyV1, changed} {
		r, err := New(Config{Source: source, Policy: policy, Cache: cache})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Popular(context.Background(), "gallery", signal.AllTime(), 10); err != nil {
			t.Fatal(err)
		}
	}
	if source.calls != 2 {
		t.Fatalf("different policy parameters reused a cached ranking; source calls=%d", source.calls)
	}
}

func TestReviewEmptyCandidatesRejectInvalidWindow(t *testing.T) {
	r, err := New(Config{Source: fakeSource{}, Policy: PolicyV1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Candidates(context.Background(), "gallery", nil, signal.Window{From: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)})
	if err == nil {
		t.Fatal("empty candidates silently accepted a non-day window")
	}
}
