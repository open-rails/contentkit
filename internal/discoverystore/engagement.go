package discoverystore

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/internal/readingstore"
	"github.com/open-rails/contentkit/internal/search"
)

// query bounds one candidate request.
type query struct {
	// ContentKinds limits candidate kinds (empty = any).
	ContentKinds []string
	// Limit is the number of results the caller returns after filtering.
	Limit int
	// SeedLimit and SeedContentKinds bound which of the reader's reads
	// inform ForSubject (0 / empty = default).
	SeedLimit        int
	SeedContentKinds []string
}

// candidate is one ranked work.
type candidate struct {
	Ref   contentref.ContentRef
	Score float64
}

// Engagement is the candidate source of similar and "for you" lists:
// ForSubject seeds from the reader's recent good reads and RRF-fuses each
// seed's similar works.
type Engagement struct {
	s *Service
	// RRFK is the seed fusion constant (default 60).
	RRFK int
}

// Similar has no candidates yet: co-engagement read ClickHouse's dropped
// engagement tables.
func (e Engagement) Similar(context.Context, contentref.ContentRef, query) ([]candidate, error) {
	return nil, nil
}

// ForSubject never returns its seeds.
func (e Engagement) ForSubject(ctx context.Context, reader uuid.UUID, q query) ([]candidate, error) {
	seedLimit := q.SeedLimit
	if seedLimit <= 0 {
		seedLimit = 5
	}
	seeds, err := e.s.topReads(ctx, reader, q.SeedContentKinds, seedLimit)
	if err != nil || len(seeds) == 0 {
		return nil, err
	}
	isSeed := make(map[contentref.ContentKey]struct{}, len(seeds))
	for _, seed := range seeds {
		isSeed[seed.Key()] = struct{}{}
	}
	lists := make([][]search.RRFKey, 0, len(seeds))
	for _, seed := range seeds {
		similar, err := e.Similar(ctx, seed, query{ContentKinds: q.ContentKinds, Limit: clampInt(q.Limit, 20, 100)})
		if err != nil {
			return nil, err
		}
		keys := make([]search.RRFKey, 0, len(similar))
		for _, c := range similar {
			keys = append(keys, search.RRFKey{ContentKey: c.Ref.Key()})
		}
		lists = append(lists, keys)
	}
	fused := search.FuseRRF(lists, search.RRFOptions{K: e.RRFK})
	out := make([]candidate, 0, len(fused))
	for _, f := range fused {
		ref := f.Ref()
		if _, ok := isSeed[ref.Key()]; !ok {
			out = append(out, candidate{Ref: ref, Score: float64(f.Score)})
		}
	}
	return out, nil
}

// topReads are the reader's most recent good or complete reads of kinds
// (any when empty), newest first, leaving out what they disliked or hid.
func (s *Service) topReads(ctx context.Context, reader uuid.UUID, kinds []string, limit int) ([]contentref.ContentRef, error) {
	rows, err := s.o.Pool.Query(ctx, `SELECT c.kind, c.id FROM `+s.t.reading+` r JOIN `+s.t.items+` c ON c.tenant_id = $1 AND c.id = r.content_id::text
 WHERE r.app_id = $2 AND r.subject = $3 AND r.best_outcome >= $4 AND NOT r.hidden AND (cardinality($5::text[]) = 0 OR c.kind = ANY($5))
  AND NOT EXISTS (SELECT 1 FROM `+s.t.reactions+` x WHERE x.tenant_id = $1 AND x.user_id = r.subject::text AND x.content_id = c.id AND x.value < 0)
 ORDER BY r.last_seen_at DESC, r.content_id LIMIT $6`, s.o.Tenant, s.app(), reader, readingstore.OutcomeGood, nonNil(kinds), limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (contentref.ContentRef, error) {
		var kind, id string
		err := row.Scan(&kind, &id)
		return contentref.New(s.o.Tenant, kind, id), err
	})
}

// seenIDs are the content ids of kind the reader opened.
func (s *Service) seenIDs(ctx context.Context, reader uuid.UUID, kind string) (map[string]struct{}, error) {
	rows, err := s.o.Pool.Query(ctx, `SELECT c.id FROM `+s.t.reading+` r JOIN `+s.t.items+` c ON c.tenant_id = $1 AND c.id = r.content_id::text
 WHERE r.app_id = $2 AND r.subject = $3 AND r.last_seen_at IS NOT NULL AND c.kind = $4`, s.o.Tenant, s.app(), reader, kind)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out, err
}

// negativeIDs are the works of kinds the reader disliked or hid.
func (s *Service) negativeIDs(ctx context.Context, reader uuid.UUID, kinds []string) (map[contentref.ContentKey]struct{}, error) {
	rows, err := s.o.Pool.Query(ctx, `SELECT x.content_kind, x.content_id FROM `+s.t.reactions+` x
 WHERE x.tenant_id = $1 AND x.user_id = $5 AND x.value < 0 AND x.content_kind = ANY($4)
UNION SELECT c.kind, c.id FROM `+s.t.reading+` r JOIN `+s.t.items+` c ON c.tenant_id = $1 AND c.id = r.content_id::text
 WHERE r.app_id = $2 AND r.subject = $3 AND r.hidden AND c.kind = ANY($4)`, s.o.Tenant, s.app(), reader, nonNil(kinds), reader.String())
	if err != nil {
		return nil, err
	}
	refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (contentref.ContentRef, error) {
		var kind, id string
		err := row.Scan(&kind, &id)
		return contentref.New(s.o.Tenant, kind, id), err
	})
	out := make(map[contentref.ContentKey]struct{}, len(refs))
	for _, ref := range refs {
		out[ref.Key()] = struct{}{}
	}
	return out, err
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// viewStates are the reader's standing with refs' works: opened, completed
// and disliked.
func (s *Service) viewStates(ctx context.Context, reader uuid.UUID, refs []contentref.ContentRef) (map[contentref.ContentKey]hub.ViewState, error) {
	ids := make([]string, len(refs))
	for i, ref := range refs {
		ids[i] = ref.ContentID
	}
	rows, err := s.o.Pool.Query(ctx, `SELECT c.kind, c.id, coalesce(r.last_seen_at IS NOT NULL, false), coalesce(r.best_outcome = $4, false),
 EXISTS (SELECT 1 FROM `+s.t.reactions+` x WHERE x.tenant_id = $1 AND x.user_id = $5 AND x.content_id = c.id AND x.value < 0)
FROM `+s.t.items+` c LEFT JOIN `+s.t.reading+` r ON r.app_id = $2 AND r.subject = $3 AND r.content_id = c.id::uuid
WHERE c.tenant_id = $1 AND c.id = ANY($6)`, s.o.Tenant, s.app(), reader, readingstore.OutcomeComplete, reader.String(), ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[contentref.ContentKey]hub.ViewState{}
	for rows.Next() {
		var kind, id string
		var v hub.ViewState
		if err := rows.Scan(&kind, &id, &v.Seen, &v.Completed, &v.Disliked); err != nil {
			return nil, err
		}
		if v.Seen || v.Disliked {
			out[contentref.New(s.o.Tenant, kind, id).Key()] = v
		}
	}
	return out, rows.Err()
}
