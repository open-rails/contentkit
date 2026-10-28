package discoverystore

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/normalize"
	"github.com/open-rails/contentkit/signal"
)

const maxSearchQuery = 200

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

// RecentSearches lists the reader's recent searches, newest first.
func (s *Service) RecentSearches(ctx context.Context, actor access.Actor, session string, limit int) (discovery.RecentSearches, error) {
	out := discovery.RecentSearches{Searches: []discovery.RecentSearch{}}
	subject, err := s.subject(actor, session)
	if err != nil {
		return out, err
	}
	rows, err := s.o.Pool.Query(ctx, `SELECT query, results, searched_at FROM `+s.t.searches+`
 WHERE tenant_id = $1 AND subject = $2 ORDER BY searched_at DESC, normalized LIMIT $3`, s.o.Tenant, subject.Key(), pageLimit(limit, 10))
	if err != nil {
		return out, err
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (discovery.RecentSearch, error) {
		var x discovery.RecentSearch
		err := r.Scan(&x.Query, &x.Results, &x.SearchedAt)
		return x, err
	})
	if err != nil {
		return out, err
	}
	out.Searches = append(out.Searches, got...)
	return out, nil
}

// ClearSearches deletes the reader's recent searches: one query, or all of
// them.
func (s *Service) ClearSearches(ctx context.Context, actor access.Actor, session, query string) error {
	subject, err := s.subject(actor, session)
	if err != nil {
		return err
	}
	if query == "" {
		_, err = s.o.Pool.Exec(ctx, `DELETE FROM `+s.t.searches+` WHERE tenant_id = $1 AND subject = $2`, s.o.Tenant, subject.Key())
		return err
	}
	key, _ := searchKey(query)
	_, err = s.o.Pool.Exec(ctx, `DELETE FROM `+s.t.searches+` WHERE tenant_id = $1 AND subject = $2 AND normalized = $3`, s.o.Tenant, subject.Key(), key)
	return err
}

// EraseSearches deletes subjects' recent searches.
func (s *Service) EraseSearches(ctx context.Context, subjects []signal.Subject) error {
	keys := make([]string, 0, len(subjects))
	for _, sub := range subjects {
		keys = append(keys, sub.Key())
	}
	_, err := s.o.Pool.Exec(ctx, `DELETE FROM `+s.t.searches+` WHERE tenant_id = $1 AND subject = ANY($2)`, s.o.Tenant, keys)
	return err
}

// PruneSearches deletes recent searches unused for SearchRetention, and any
// of an erased person (one recorded while the erasure ran). It returns how
// many it deleted.
func (s *Service) PruneSearches(ctx context.Context) (int, error) {
	var n int64
	for _, q := range []string{
		`DELETE FROM ` + s.t.searches + ` x USING ` + s.t.erased + ` e WHERE x.tenant_id = $1 AND e.tenant_id = x.tenant_id AND e.actor_id = x.subject`,
		`DELETE FROM ` + s.t.searches + ` WHERE tenant_id = $1 AND searched_at < now() - make_interval(secs => ` +
			strconv.Itoa(int(discovery.SearchRetention.Seconds())) + `)`,
	} {
		tag, err := s.o.Pool.Exec(ctx, q, s.o.Tenant)
		if err != nil {
			return int(n), err
		}
		n += tag.RowsAffected()
	}
	return int(n), nil
}

var sessionParam = httpapi.Text("session", "a signed-out reader's session, which keeps its recent searches")

var searchRoutes = []httpapi.Route[handler]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/me/searches", Resource: "discovery", Auth: httpapi.Public,
		Doc:   "The reader's recent searches, newest first, since they last cleared them; a signed-out reader names its session.",
		Query: []httpapi.Param{sessionParam, httpapi.Int("limit", "10 by default, at most 100")}, Responses: []httpapi.Reply{httpapi.OK(discovery.RecentSearches{})}},
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
func (h handler) recordSearch(r *http.Request, o SearchOptions, page discovery.SearchPage) {
	if o.Cursor != "" || o.Page > 1 {
		return
	}
	if err := h.s.RecordSearch(r.Context(), o.Actor, r.URL.Query().Get("session"), o.Query, len(page.Contents)); err != nil && !errors.Is(err, context.Canceled) {
		h.s.o.Logger.WarnContext(r.Context(), "discovery: recent search not kept", "err", err)
	}
}
