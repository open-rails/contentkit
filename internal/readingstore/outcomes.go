package readingstore

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/internal/outcomevalue"
)

// SeedOutcomeValues writes outcomevalue's starting guesses for app where its rows
// are missing; a refit's rows are kept.
func SeedOutcomeValues(ctx context.Context, pool *pgxpool.Pool, schema string, app uuid.UUID) error {
	seed := outcomevalue.Seed()
	segments, outcomes := make([]string, len(seed)), make([]string, len(seed))
	ps, qs := make([]float64, len(seed)), make([]float64, len(seed))
	for i, r := range seed {
		segments[i], outcomes[i], ps[i], qs[i] = r.Segment, r.Outcome, r.PSequel, r.Q
	}
	_, err := pool.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "outcome_values"}.Sanitize()+` (app_id, segment, outcome, p_sequel, q)
SELECT $1::uuid, u.segment, u.outcome, u.p, u.q FROM unnest($2::text[], $3::text[], $4::float8[], $5::float8[]) AS u(segment, outcome, p, q)
ON CONFLICT (app_id, segment, outcome) DO NOTHING`, app, segments, outcomes, ps, qs)
	return err
}
