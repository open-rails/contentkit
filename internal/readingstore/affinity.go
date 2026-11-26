package readingstore

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/internal/outcomevalue"
)

// AffinityWindow is how far back affinity reads a person's reading.
const AffinityWindow = 365 * 24 * time.Hour

// Tags lists, for each of contents, the tags of tags it carries.
func (s *Store) Tags(ctx context.Context, contents []uuid.UUID, tags []string) (map[uuid.UUID][]string, error) {
	out := map[uuid.UUID][]string{}
	if len(contents) == 0 || len(tags) == 0 {
		return out, nil
	}
	texts := make([]string, len(contents))
	for i, id := range contents {
		texts[i] = id.String()
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT content_id::uuid, entity_id FROM `+s.t.memberships+`
 WHERE tenant_id = $1 AND content_id = ANY($2) AND entity_id = ANY($3) AND state = 'active'`, s.tenant, texts, tags)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return nil, err
		}
		out[id] = append(out[id], tag)
	}
	return out, rows.Err()
}

// Affine reports, per person, the tags they have positive affinity for: a
// positive mean q over their reads since since of galleries carrying the
// tag, each read valued by its best outcome and their reaction. segment is a
// content kind's segment.
func (s *Store) Affine(ctx context.Context, people []uuid.UUID, tags []string, since time.Time, values outcomevalue.Table, segment func(kind string) string) (map[uuid.UUID]map[string]bool, error) {
	out := map[uuid.UUID]map[string]bool{}
	if len(people) == 0 || len(tags) == 0 {
		return out, nil
	}
	texts := make([]string, len(people))
	for i, id := range people {
		texts[i] = id.String()
	}
	rows, err := s.pool.Query(ctx, `SELECT r.subject, m.entity_id, c.kind, r.best_outcome, r.hidden,
 EXISTS (SELECT 1 FROM `+s.t.favorites+` f WHERE f.app_id = $2 AND f.user_id = r.subject AND f.content_id = r.content_id AND f.value = 1),
 coalesce((SELECT x.value FROM `+s.t.reactions+` x WHERE x.tenant_id = $1 AND x.content_id = r.content_id::text AND x.value <> 0
   AND (x.user_id = r.subject::text OR x.user_id IS NULL AND x.anonymous_subject = r.subject) ORDER BY x.updated_at DESC LIMIT 1), 0)
FROM `+s.t.state+` r
JOIN `+s.t.items+` c ON c.tenant_id = $1 AND c.id = r.content_id::text
JOIN `+s.t.memberships+` m ON m.tenant_id = $1 AND m.content_id = c.id AND m.state = 'active' AND m.entity_id = ANY($5)
WHERE r.app_id = $2 AND r.subject = ANY($3::text[]::uuid[]) AND r.last_seen_at >= $4
GROUP BY r.subject, r.content_id, m.entity_id, c.kind, r.best_outcome, r.hidden`, s.tenant, s.app, texts, since, tags)
	if err != nil {
		return nil, err
	}
	type key struct {
		person uuid.UUID
		tag    string
	}
	sum := map[key]float64{}
	err = forEach(rows, func(r pgx.Rows) error {
		var person uuid.UUID
		var tag, kind string
		var outcome int16
		var hidden, favorite bool
		var reacted int16
		if err := r.Scan(&person, &tag, &kind, &outcome, &hidden, &favorite, &reacted); err != nil {
			return err
		}
		reaction := ""
		switch {
		case hidden:
			reaction = outcomevalue.Hidden
		case favorite:
			reaction = outcomevalue.Favorite
		case reacted > 0:
			reaction = outcomevalue.Like
		case reacted < 0:
			reaction = outcomevalue.Dislike
		}
		sum[key{person, tag}] += values.Of(segment(kind), className(outcome), reaction, "")
		return nil
	})
	if err != nil {
		return nil, err
	}
	for k, v := range sum {
		if v > 0 {
			if out[k.person] == nil {
				out[k.person] = map[string]bool{}
			}
			out[k.person][k.tag] = true
		}
	}
	return out, nil
}

func className(outcome int16) string {
	switch outcome {
	case OutcomeBounce:
		return outcomevalue.Bounce
	case OutcomeNeutral:
		return outcomevalue.Neutral
	case OutcomeGood:
		return outcomevalue.Good
	case OutcomeComplete:
		return outcomevalue.Complete
	}
	return ""
}

func forEach(rows pgx.Rows, f func(pgx.Rows) error) error {
	defer rows.Close()
	for rows.Next() {
		if err := f(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
