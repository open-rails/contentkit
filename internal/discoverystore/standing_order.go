package discoverystore

import (
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	contentstore "github.com/open-rails/contentkit/internal/contentstore"
	"github.com/open-rails/contentkit/signal"
)

// standingOrder is the explicit fallback when readership cannot be normalized:
// each person's current reaction or favorite on any edition, last changed in
// the window, is one vote scored by its Wilson lower bound. It reads no cached
// ranks, imported counters or inferred reading, and cannot recover votes later
// removed; the ranking names that meaning.
func (s *Service) standingOrder(raw string, w signal.Window, favorites bool) order {
	// Erasure deletes these rows; the fence also keeps restored residue out.
	erased := fmt.Sprintf(`NOT EXISTS (SELECT 1 FROM %s e WHERE e.tenant_id=a.tenant_id AND e.actor_id=a.user_id)`, s.t.erased)
	window := ` AND (@standing_from::timestamptz IS NULL OR a.updated_at>=@standing_from) AND (@standing_until::timestamptz IS NULL OR a.updated_at<@standing_until)`
	action := fmt.Sprintf(`SELECT CASE WHEN a.user_id IS NOT NULL THEN 'user:'||a.user_id WHEN a.anonymous_actor_id IS NOT NULL THEN 'anon:'||a.anonymous_actor_id::text ELSE 'guest:'||a.ip END AS actor,a.value AS vote FROM %s a
 WHERE a.tenant_id=f.tenant_id AND a.content_kind=f.kind AND a.content_id=f.content_id AND a.value<>0 AND NOT a.author_hidden AND (a.user_id IS NULL OR %s)`+window, s.t.reactions, erased)
	if favorites {
		action += fmt.Sprintf(` UNION ALL SELECT CASE WHEN a.user_id IS NOT NULL THEN 'user:'||a.user_id ELSE 'anon:'||a.anonymous_actor_id::text END,a.value FROM %s a
 WHERE a.tenant_id=f.tenant_id AND a.content_kind=f.kind AND a.content_id=f.content_id AND a.value=1 AND %s`+window, s.t.favorites, erased)
	}
	// Any positive action makes the person a supporter; one person, one vote.
	join := `LEFT JOIN LATERAL (SELECT count(*) FILTER (WHERE vote>0) AS positive,count(*) FILTER (WHERE vote<0) AS negative
 FROM (SELECT actor,max(vote) AS vote FROM (` + action + `) actions GROUP BY actor) people) standing ON true`
	var from, until *time.Time
	if !w.From.IsZero() {
		v := w.From.UTC()
		from = &v
	}
	if !w.To.IsZero() {
		v := w.To.UTC()
		until = &v
	}
	score := `CASE WHEN standing.positive=0 THEN 0::float8 ELSE ` + contentstore.RatingSQL("standing.positive", "standing.negative") + ` END`
	return order{raw: raw, expr: score, join: join, desc: true, cast: "float8", args: pgx.NamedArgs{"standing_from": from, "standing_until": until}}
}
