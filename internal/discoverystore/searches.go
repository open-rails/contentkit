package discoverystore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/normalize"
	signalstore "github.com/open-rails/contentkit/internal/signalstore"
	"github.com/open-rails/contentkit/internal/subject"
)

const maxSearchQuery = 200

func searchKey(query string) (string, bool) {
	q := strings.ToLower(normalize.Query(query))
	return q, q != "" && utf8.RuneCountInString(q) <= maxSearchQuery
}

// RecordSearch keeps a first page's query among the reader's recent searches
// and logs it with its results in order; a signed-out reader without an
// anonymous id, or an erased person, keeps none.
func (s *Service) RecordSearch(r *http.Request, actor access.Actor, query string, results []string, requestID uuid.UUID) error {
	key, ok := searchKey(query)
	if !ok {
		return nil
	}
	who, ok := s.subjectKey(actor, r.Header.Get(subject.Header))
	if !ok {
		return nil
	}
	if row, ok := s.o.Activity.Row(r, actor, r.URL.Query().Get("language")); ok {
		row.Action, row.Query, row.RequestID = signalstore.ActionSearch, key, requestID
		for _, id := range results {
			if u, err := uuid.Parse(id); err == nil {
				row.Results = append(row.Results, u)
			}
		}
		s.o.Activity.Add(row)
	}
	_, err := s.o.Pool.Exec(r.Context(), `INSERT INTO `+s.t.searches+` (tenant_id, subject, normalized, query, results)
 SELECT $1, $2, $3, $4, $5 WHERE NOT EXISTS (SELECT 1 FROM `+s.t.erased+` WHERE tenant_id = $1 AND actor_id = $2)
 ON CONFLICT (tenant_id, subject, normalized) DO UPDATE SET query = EXCLUDED.query, results = EXCLUDED.results,
  searches = `+s.t.searches+`.searches + 1, searched_at = now()`, s.o.Tenant, who, key, strings.TrimSpace(query), len(results))
	if id, perr := uuid.Parse(who); err == nil && perr == nil && s.o.Reading != nil {
		err = s.o.Reading.Touch(r.Context(), s.o.Pool, id)
	}
	return err
}

// RecentSearches lists the reader's recent searches, newest first.
func (s *Service) RecentSearches(ctx context.Context, actor access.Actor, anonymousID string, limit int) (discovery.RecentSearches, error) {
	out := discovery.RecentSearches{Searches: []discovery.RecentSearch{}}
	who, ok := s.subjectKey(actor, anonymousID)
	if !ok {
		return out, nil
	}
	rows, err := s.o.Pool.Query(ctx, `SELECT query, results, searched_at FROM `+s.t.searches+`
 WHERE tenant_id = $1 AND subject = $2 ORDER BY searched_at DESC, normalized LIMIT $3`, s.o.Tenant, who, pageLimit(limit, 10))
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
func (s *Service) ClearSearches(ctx context.Context, actor access.Actor, anonymousID, query string) error {
	who, ok := s.subjectKey(actor, anonymousID)
	if !ok {
		return fmt.Errorf("%w: a signed-out reader sends its anonymous id in %s", ErrInvalid, subject.Header)
	}
	if query == "" {
		_, err := s.o.Pool.Exec(ctx, `DELETE FROM `+s.t.searches+` WHERE tenant_id = $1 AND subject = $2`, s.o.Tenant, who)
		return err
	}
	key, _ := searchKey(query)
	_, err := s.o.Pool.Exec(ctx, `DELETE FROM `+s.t.searches+` WHERE tenant_id = $1 AND subject = $2 AND normalized = $3`, s.o.Tenant, who, key)
	return err
}

// EraseSearches deletes an account's recent searches.
func (s *Service) EraseSearches(ctx context.Context, accountID string) error {
	_, err := s.o.Pool.Exec(ctx, `DELETE FROM `+s.t.searches+` WHERE tenant_id = $1 AND subject = $2`, s.o.Tenant, accountID)
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

var searchRoutes = []httpapi.Route[handler]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/me/searches", Resource: "discovery", Auth: httpapi.Public,
		Doc:   "The reader's recent searches, newest first, since they last cleared them; a signed-out reader's are its anonymous id's (ContentKit-Anonymous-Id).",
		Query: []httpapi.Param{httpapi.Int("limit", "10 by default, at most 100")}, Responses: []httpapi.Reply{httpapi.OK(discovery.RecentSearches{})}},
		Serve: httpapi.H(handler.recentSearches)},
	{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/me/searches", Resource: "discovery", Auth: httpapi.Public,
		Doc:   "Clears the reader's recent searches: one query, or every one so far.",
		Query: []httpapi.Param{httpapi.Text("query", "only this query")}, Responses: []httpapi.Reply{httpapi.NoContent}},
		Serve: httpapi.H(handler.clearSearches)},
}

func (h handler) recentSearches(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.s.RecentSearches(r.Context(), gate.Actor(r.Context()), r.Header.Get(subject.Header), limit)
	h.reply(w, r, out, err)
}

func (h handler) clearSearches(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	h.done(w, r, h.s.ClearSearches(r.Context(), gate.Actor(r.Context()), r.Header.Get(subject.Header), q.Get("query")))
}

// recordSearch keeps and logs a first page's query, naming it in the page's
// request_id; a failure only logs.
func (h handler) recordSearch(r *http.Request, o SearchOptions, page *discovery.SearchPage) {
	if o.Cursor != "" || o.Page > 1 {
		return
	}
	requestID := uuid.MustParse(contentref.NewID())
	page.RequestID = requestID.String()
	results := make([]string, len(page.Contents))
	for i, c := range page.Contents {
		results[i] = c.ID
	}
	if err := h.s.RecordSearch(r, o.Actor, o.Query, results, requestID); err != nil && !errors.Is(err, context.Canceled) {
		h.s.o.Logger.WarnContext(r.Context(), "discovery: recent search not kept", "err", err)
	}
}
