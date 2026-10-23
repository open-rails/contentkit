package groups

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/kinds"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
)

type idleOwned struct{}

func (idleOwned) DeleteOwned(context.Context, []string) error  { return nil }
func (idleOwned) RestoreOwned(context.Context, []string) error { return nil }

func TestLifecycleSchedulesRequireConfiguredKinds(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind kinds.GroupKind
		want int
	}{
		{"ordinary taxonomy", kinds.GroupKind{}, 0},
		{"timed curation", kinds.GroupKind{Windows: true}, 0},
		{"owned channel", kinds.GroupKind{Owner: "user"}, 1},
		{"permission scope", kinds.GroupKind{AuthScope: true}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, err := pgxpool.New(t.Context(), "postgres://unused@127.0.0.1:1/unused?sslmode=disable")
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			s := &Store{tenant: "site", queue: "contentkit_groups", river: &riverBinding{}, owned: idleOwned{}, scopes: scopeCandidates{}, cfg: kinds.Config{GroupKinds: map[string]kinds.GroupKind{"entity": tc.kind}}}
			observed := -1
			inspect := riverhelpers.NewContribution("inspect", func(_ context.Context, cfg *river.Config) error { observed = len(cfg.PeriodicJobs); return nil }, nil, nil)
			if _, err := riverhelpers.New(t.Context(), pool, &river.Config{}, s.RiverJobs(), inspect); err != nil {
				t.Fatal(err)
			}
			if observed != tc.want {
				t.Fatalf("periodic jobs=%d want=%d", observed, tc.want)
			}
		})
	}
}
