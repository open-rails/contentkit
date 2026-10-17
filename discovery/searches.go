package discovery

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/normalize"
	"github.com/open-rails/contentkit/signal"
)

// Recent searches: what a reader (a signed-in person, or a signed-out
// session) searched, one entry per query, newest first. Clearing them sets
// a hide-before marker; the prune job deletes hidden, erased and expired
// entries.

// SearchRetention is how long a recent search is kept after its last use.
const SearchRetention = 180 * 24 * time.Hour

const maxSearchQuery = 200 // runes

// RecentSearch is one query the reader searched.
type RecentSearch struct {
	Query string `json:"query"`
	// Results is how many cards its first page showed.
	Results    int       `json:"results"`
	SearchedAt time.Time `json:"searched_at"`
}

// RecentSearches is the reader's recent searches, newest first.
type RecentSearches struct {
	Searches []RecentSearch `json:"searches"`
}

func searchKey(query string) (string, bool) {
	q := strings.ToLower(normalize.Query(query))
	return q, q != "" && utf8.RuneCountInString(q) <= maxSearchQuery
}

// RecordSearch keeps a query the reader searched; a signed-out reader without
// a session, or an erased person, keeps none.
func (s *Service) RecordSearch(ctx context.Context, actor access.Actor, session, query string, results int) error {
	key, ok := searchKey(query)
	if !ok {
		return nil
	}
	subject, err := s.subject(actor, session)
	if err != nil {
		return nil
	}
	_, err = s.o.Pool.Exec(ctx, `INSERT INTO `+s.t.searches+` (tenant_id, subject, normalized, query, results)
 SELECT $1, $2, $3, $4, $5 WHERE NOT EXISTS (SELECT 1 FROM `+s.t.erased+` WHERE tenant_id = $1 AND actor_id = $2)
 ON CONFLICT (tenant_id, subject, normalized) DO UPDATE SET query = EXCLUDED.query, results = EXCLUDED.results,
  searches = `+s.t.searches+`.searches + 1, searched_at = now()`, s.o.Tenant, subject.Key(), key, strings.TrimSpace(query), results)
	return err
}

// RecentSearches lists the reader's recent searches since their last clear.
func (s *Service) RecentSearches(ctx context.Context, actor access.Actor, session string, limit int) (RecentSearches, error) {
	out := RecentSearches{Searches: []RecentSearch{}}
	subject, err := s.subject(actor, session)
	if err != nil {
		return out, err
	}
	rows, err := s.o.Pool.Query(ctx, `SELECT x.query, x.results, x.searched_at FROM `+s.t.searches+` x
 LEFT JOIN `+s.t.searchMarks+` m ON m.tenant_id = x.tenant_id AND m.subject = x.subject
 WHERE x.tenant_id = $1 AND x.subject = $2 AND (m.hidden_before IS NULL OR x.searched_at >= m.hidden_before)
 ORDER BY x.searched_at DESC, x.normalized LIMIT $3`, s.o.Tenant, subject.Key(), pageLimit(limit, 10))
	if err != nil {
		return out, err
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RecentSearch, error) {
		var x RecentSearch
		return x, r.Scan(&x.Query, &x.Results, &x.SearchedAt)
	})
	if err != nil {
		return out, err
	}
	out.Searches = append(out.Searches, got...)
	return out, nil
}

// ClearSearches hides the reader's recent searches: one query, or all of
// them until now.
func (s *Service) ClearSearches(ctx context.Context, actor access.Actor, session, query string) error {
	subject, err := s.subject(actor, session)
	if err != nil {
		return err
	}
	if query != "" {
		key, _ := searchKey(query)
		_, err := s.o.Pool.Exec(ctx, `DELETE FROM `+s.t.searches+` WHERE tenant_id = $1 AND subject = $2 AND normalized = $3`, s.o.Tenant, subject.Key(), key)
		return err
	}
	_, err = s.o.Pool.Exec(ctx, `INSERT INTO `+s.t.searchMarks+` (tenant_id, subject, hidden_before) VALUES ($1, $2, now())
 ON CONFLICT (tenant_id, subject) DO UPDATE SET hidden_before = greatest(`+s.t.searchMarks+`.hidden_before, EXCLUDED.hidden_before)`, s.o.Tenant, subject.Key())
	return err
}

// EraseSearches deletes subjects' recent searches and markers.
func (s *Service) EraseSearches(ctx context.Context, subjects []signal.Subject) error {
	keys := make([]string, 0, len(subjects))
	for _, sub := range subjects {
		keys = append(keys, sub.Key())
	}
	return pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		for _, t := range []string{s.t.searches, s.t.searchMarks} {
			if _, err := tx.Exec(ctx, `DELETE FROM `+t+` WHERE tenant_id = $1 AND subject = ANY($2)`, s.o.Tenant, keys); err != nil {
				return err
			}
		}
		return nil
	})
}

// PruneSearches deletes recent searches hidden by a clear, of erased
// people, or unused for SearchRetention, then the markers they no longer
// need. It returns how many searches it deleted.
func (s *Service) PruneSearches(ctx context.Context) (int, error) {
	var n int64
	err := pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		for _, q := range []string{
			`DELETE FROM ` + s.t.searches + ` x USING ` + s.t.searchMarks + ` m
 WHERE x.tenant_id = $1 AND m.tenant_id = x.tenant_id AND m.subject = x.subject AND x.searched_at < m.hidden_before`,
			`DELETE FROM ` + s.t.searches + ` x USING ` + s.t.erased + ` e WHERE x.tenant_id = $1 AND e.tenant_id = x.tenant_id AND e.actor_id = x.subject`,
			`DELETE FROM ` + s.t.searches + ` WHERE tenant_id = $1 AND searched_at < now() - make_interval(secs => ` +
				strconv.Itoa(int(SearchRetention.Seconds())) + `)`,
		} {
			tag, err := tx.Exec(ctx, q, s.o.Tenant)
			if err != nil {
				return err
			}
			n += tag.RowsAffected()
		}
		_, err := tx.Exec(ctx, `DELETE FROM `+s.t.searchMarks+` m WHERE m.tenant_id = $1 AND NOT EXISTS (SELECT 1 FROM `+s.t.searches+` x
 WHERE x.tenant_id = m.tenant_id AND x.subject = m.subject AND x.searched_at < m.hidden_before)`, s.o.Tenant)
		return err
	})
	return int(n), err
}

var sessionParam = httpapi.Text("session", "a signed-out reader's session, which keeps its recent searches")

var searchRoutes = []httpapi.Route[handler]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/me/searches", Resource: "discovery", Auth: httpapi.Public,
		Doc:   "The reader's recent searches, newest first, since they last cleared them; a signed-out reader names its session.",
		Query: []httpapi.Param{sessionParam, httpapi.Int("limit", "10 by default, at most 100")}, Responses: []httpapi.Reply{httpapi.OK(RecentSearches{})}},
		Serve: httpapi.H(handler.recentSearches)},
	{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/me/searches", Resource: "discovery", Auth: httpapi.Public,
		Doc:   "Clears the reader's recent searches: one query, or every one so far.",
		Query: []httpapi.Param{sessionParam, httpapi.Text("query", "only this query")}, Responses: []httpapi.Reply{httpapi.NoContent}},
		Serve: httpapi.H(handler.clearSearches)},
}

func (h handler) recentSearches(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.s.RecentSearches(r.Context(), gate.Actor(r.Context()), r.URL.Query().Get("session"), limit)
	h.reply(w, r, out, err)
}

func (h handler) clearSearches(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	h.done(w, r, h.s.ClearSearches(r.Context(), gate.Actor(r.Context()), q.Get("session"), q.Get("query")))
}

// recordSearch keeps a first page's query; a failure only logs.
func (h handler) recordSearch(r *http.Request, o SearchOptions, page ContentPage) {
	if o.Cursor != "" || o.Page > 1 {
		return
	}
	if err := h.s.RecordSearch(r.Context(), o.Actor, r.URL.Query().Get("session"), o.Query, len(page.Contents)); err != nil && !errors.Is(err, context.Canceled) {
		h.s.o.Logger.WarnContext(r.Context(), "discovery: recent search not kept", "err", err)
	}
}
