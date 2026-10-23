package entities

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/kinds"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
)

func TestEntityJobsReadPreviouslyQueuedEnvelopes(t *testing.T) {
	var args syncArgs
	if err := json.Unmarshal([]byte(`{"tenant":"site","groups":["existing-entity"]}`), &args); err != nil {
		t.Fatal(err)
	}
	if args.Tenant != "site" || !slices.Equal(args.Entities, []string{"existing-entity"}) {
		t.Fatalf("queued entity IDs lost: %+v", args)
	}
	if args.Kind() != "contentkit_entity_sync" || !slices.Contains(args.KindAliases(), "contentkit_group_sync") {
		t.Fatal("old sync jobs cannot dispatch to the renamed worker")
	}
	if (reconcileArgs{}).Kind() != "contentkit_entity_reconcile" || !slices.Contains((reconcileArgs{}).KindAliases(), "contentkit_group_reconcile") {
		t.Fatal("old reconcile jobs cannot dispatch to the renamed worker")
	}
}

type idleOwned struct{}

func (idleOwned) DeleteOwned(context.Context, []string) error  { return nil }
func (idleOwned) RestoreOwned(context.Context, []string) error { return nil }

func TestLifecycleSchedulesRequireConfiguredKinds(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind kinds.EntityKind
		want int
	}{
		{"ordinary taxonomy", kinds.EntityKind{}, 0},
		{"timed curation", kinds.EntityKind{Windows: true}, 0},
		{"owned channel", kinds.EntityKind{Owner: "user"}, 1},
		{"permission scope", kinds.EntityKind{AuthScope: true}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, err := pgxpool.New(t.Context(), "postgres://unused@127.0.0.1:1/unused?sslmode=disable")
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			s := &Store{tenant: "site", queue: "contentkit_groups", river: &riverBinding{}, owned: idleOwned{}, scopes: scopeCandidates{}, cfg: kinds.Config{EntityKinds: map[string]kinds.EntityKind{"entity": tc.kind}}}
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
