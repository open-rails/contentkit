package discovery

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/popularity"
	"github.com/open-rails/contentkit/signal"
)

// Activity is a kind's view activity per UTC day, newest first.
type Activity struct {
	Days []ActivityDay `json:"days"`
}

// ActivityDay is one day: view sessions, the distinct viewers and the
// signed-in ones among them.
type ActivityDay struct {
	Date    string `json:"date"`
	Views   uint64 `json:"views"`
	Viewers uint64 `json:"viewers"`
	Users   uint64 `json:"users"`
}

// TopContents ranks a kind's live, listed content by a metric over a
// period, highest first.
type TopContents struct {
	Contents []RankedContent `json:"contents"`
}

// RankedContent is one ranked content item and its metric's value.
type RankedContent struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Path  string `json:"path,omitempty"`
	Value int64  `json:"value"`
}

// MyStats is the signed-in reader's account statistics.
type MyStats struct {
	// Completed counts the contents they finished, per kind; empty without
	// a signal plane.
	Completed map[string]int64 `json:"completed"`
	// AccessSeconds is elapsed access to each configured tier, from billing's
	// recorded product windows and entitlement history. Overlaps count once;
	// future access does not count. Absent when history is unavailable.
	AccessSeconds map[string]int64 `json:"access_seconds,omitempty"`
	// Comments counts their published comments; CommentLikes and
	// CommentDislikes the reactions those received.
	Comments        int64 `json:"comments"`
	CommentLikes    int64 `json:"comment_likes"`
	CommentDislikes int64 `json:"comment_dislikes"`
}

// Top metrics.
const (
	MetricViews    = "views"
	MetricComments = "comments"
)

// topCandidates bounds the most viewed contents read from the signal plane
// before the access and listing filters keep a page of them.
const topCandidates = 1000

var statsRoutes = []httpapi.Route[handler]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/stats/activity", Resource: "stats", Auth: httpapi.Staff, Perm: string(access.ContentViewRestricted),
		Doc:       "A content kind's views per UTC day, newest first: view sessions, distinct viewers and signed-in viewers, from the signal plane.",
		Query:     []httpapi.Param{httpapi.Text("kind", "the content kind; required"), httpapi.Int("days", "the last days, today included: 7 by default, at most 90")},
		Responses: []httpapi.Reply{httpapi.OK(Activity{})}, Errors: signals}, Serve: httpapi.H(handler.activity)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/stats/top", Resource: "stats", Auth: httpapi.Staff, Perm: string(access.ContentViewRestricted),
		Doc: "A content kind's live, listed content in the request's site ranked by views (the signal plane's) or published comments over a period.",
		Query: []httpapi.Param{httpapi.Text("kind", "the content kind; required"), httpapi.Text("metric", "views (the default) or comments"),
			httpapi.Text("period", "7d, 30d (the default), 90d, 365d or all"),
			httpapi.List("access", "public, tier, group, ppv or group_ppv, as lists filter"), httpapi.List("tiers", "configured tiers, as lists filter"),
			httpapi.Text("language", "the language of titles and paths; the site's first by default"), httpapi.Int("limit", "10 by default, at most 100")},
		Responses: []httpapi.Reply{httpapi.OK(TopContents{})}, Errors: signals}, Serve: httpapi.H(handler.top)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/me/stats", Resource: "stats", Auth: httpapi.User,
		Doc:       "The signed-in reader's account statistics: the contents they finished per kind, their comments and the reactions those received, and elapsed access seconds per tier when billing supplies its history.",
		Responses: []httpapi.Reply{httpapi.OK(MyStats{})}}, Serve: httpapi.H(handler.myStats)},
}

func (h handler) activity(w http.ResponseWriter, r *http.Request) {
	days, err := intParam(r, "days")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.s.Activity(r.Context(), r.URL.Query().Get("kind"), days)
	h.reply(w, r, out, err)
}

func (h handler) top(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	out, err := h.s.Top(r.Context(), TopOptions{Host: r.Host, Kind: q.Get("kind"), Metric: q.Get("metric"), Period: q.Get("period"),
		Access: list(q.Get("access")), Tiers: list(q.Get("tiers")), Language: q.Get("language"), Limit: limit})
	h.reply(w, r, out, err)
}

func (h handler) myStats(w http.ResponseWriter, r *http.Request) {
	out, err := h.s.MyStats(r.Context(), gate.Actor(r.Context()))
	h.reply(w, r, out, err)
}

// Activity reads a kind's views per day of the last days (7 by default, at
// most 90).
func (s *Service) Activity(ctx context.Context, kind string, days int) (Activity, error) {
	out := Activity{Days: []ActivityDay{}}
	if _, ok := s.o.Kinds.Kinds[kind]; !ok {
		return out, fmt.Errorf("%w: kind %q is not configured", ErrInvalid, kind)
	}
	switch {
	case days == 0:
		days = 7
	case days > 90:
		return out, fmt.Errorf("%w: days is at most 90", ErrInvalid)
	}
	if err := s.signals(); err != nil {
		return out, err
	}
	got, err := s.o.Hub.Activity(ctx, kind, signal.LastDays(days, s.o.Now()))
	if err != nil {
		return out, err
	}
	for _, d := range got {
		out.Days = append(out.Days, ActivityDay{Date: d.Day.Format(time.DateOnly), Views: d.Views, Viewers: d.Viewers, Users: d.Users})
	}
	return out, nil
}

// TopOptions select a ranking.
type TopOptions struct {
	// Host picks the site scope.
	Host          string
	Kind, Metric  string
	Period        string
	Access, Tiers []string
	Language      string
	Limit         int
}

// Top ranks a kind's live, listed content in the site by views or published
// comments over a period, filtered by access as lists are.
func (s *Service) Top(ctx context.Context, o TopOptions) (TopContents, error) {
	out := TopContents{Contents: []RankedContent{}}
	if _, ok := s.o.Kinds.Kinds[o.Kind]; !ok {
		return out, fmt.Errorf("%w: kind %q is not configured", ErrInvalid, o.Kind)
	}
	o.Period = cmpOr(o.Period, popularity.DefaultPeriod)
	window, err := popularity.WindowForPeriod(o.Period, s.o.Now())
	if err != nil {
		return out, fmt.Errorf("%w: period %q is one of %v", ErrInvalid, o.Period, popularity.Periods)
	}
	switch {
	case o.Limit == 0:
		o.Limit = 10
	case o.Limit > 100:
		return out, fmt.Errorf("%w: limit is at most 100", ErrInvalid)
	}
	language, err := s.displayLanguage(o.Language)
	if err != nil {
		return out, err
	}
	v, err := s.o.Visibility.Viewer(ctx, catalog.ViewerOptions{Actor: access.Actor{Anonymous: true}, Host: o.Host})
	if err != nil {
		return out, err
	}
	pred, args := v.Indexable("f")
	q := &sel{args: args, where: []string{"f.tenant_id = @d_tenant", "f.content_id = r.id", pred}}
	if err := s.accessFilter(q, o.Access, o.Tiers); err != nil {
		return out, err
	}
	args["d_tenant"], args["d_kind"], args["d_limit"] = s.o.Tenant, o.Kind, o.Limit
	var ranked []RankedContent
	switch cmpOr(o.Metric, MetricViews) {
	case MetricViews:
		if err := s.signals(); err != nil {
			return out, err
		}
		hits, err := s.o.Hub.Popular(ctx, o.Kind, signal.PopularOptions{Window: window, Limit: topCandidates, RankExpr: "toFloat64(views)"})
		if err != nil {
			return out, err
		}
		ids, views := make([]string, len(hits)), make([]int64, len(hits))
		for i, h := range hits {
			ids[i], views[i] = h.ContentID, int64(h.Views)
		}
		args["d_ids"], args["d_values"] = ids, views
		ranked, err = s.ranked(ctx, `SELECT r.id, r.value FROM unnest(@d_ids::text[], @d_values::bigint[]) WITH ORDINALITY AS r(id, value, n)
 WHERE EXISTS (SELECT 1 FROM `+s.t.facets+` f WHERE `+q.predicate()+`) ORDER BY r.n LIMIT @d_limit`, args)
		if err != nil {
			return out, err
		}
	case MetricComments:
		var from *time.Time
		if !window.From.IsZero() {
			from = &window.From
		}
		args["d_from"] = from
		ranked, err = s.ranked(ctx, `SELECT r.id, r.value FROM (SELECT c.content_id AS id, count(*) AS value FROM `+s.t.comments+` c
 WHERE c.tenant_id = @d_tenant AND c.content_kind = @d_kind AND c.deleted_at IS NULL AND c.moderation = 'approved'
  AND (@d_from::timestamptz IS NULL OR c.created_at >= @d_from::timestamptz) GROUP BY c.content_id) r
 WHERE EXISTS (SELECT 1 FROM `+s.t.facets+` f WHERE `+q.predicate()+`) ORDER BY r.value DESC, r.id LIMIT @d_limit`, args)
		if err != nil {
			return out, err
		}
	default:
		return out, fmt.Errorf("%w: metric %q is views or comments", ErrInvalid, o.Metric)
	}
	if err := s.name(ctx, o.Kind, language, ranked); err != nil {
		return out, err
	}
	out.Contents = ranked
	return out, nil
}

func (s *Service) ranked(ctx context.Context, sql string, args pgx.NamedArgs) ([]RankedContent, error) {
	rows, err := s.o.Pool.Query(ctx, sql, args)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (RankedContent, error) {
		var c RankedContent
		return c, r.Scan(&c.ID, &c.Value)
	})
}

// name fills ranked contents' titles and paths in language.
func (s *Service) name(ctx context.Context, kind, language string, ranked []RankedContent) error {
	if len(ranked) == 0 {
		return nil
	}
	language = contenturl.Prefix(s.languages(), language)
	ids := make([]string, len(ranked))
	for i, c := range ranked {
		ids[i] = c.ID
	}
	rows, err := s.o.Pool.Query(ctx, `SELECT c.id, coalesce(t.title, ''), coalesce(cd.code, ''), coalesce(cd.slugs->>$3, cd.slug, '')
FROM `+s.t.items+` c
LEFT JOIN `+s.t.codes+` cd ON cd.tenant_id = c.tenant_id AND cd.content_kind = c.kind AND cd.content_id = c.id
LEFT JOIN LATERAL (SELECT x.title FROM `+s.t.text+` x WHERE x.tenant_id = c.tenant_id AND x.content_id = c.id
 ORDER BY x.language = $3 DESC, array_position($4::text[], x.language), x.language LIMIT 1) t ON true
WHERE c.tenant_id = $1 AND c.id = ANY($2)`, s.o.Tenant, ids, language, s.languages())
	if err != nil {
		return err
	}
	defer rows.Close()
	route := s.o.Kinds.Kinds[kind].Route
	for rows.Next() {
		var id, title, code, slug string
		if err := rows.Scan(&id, &title, &code, &slug); err != nil {
			return err
		}
		for i := range ranked {
			if ranked[i].ID != id {
				continue
			}
			ranked[i].Title = title
			if route != "" && code != "" {
				ranked[i].Path = contenturl.Path(s.languages(), language, route, contenturl.Code(code), slug)
			}
		}
	}
	return rows.Err()
}

// MyStats reads a person's account statistics; finished contents only with
// a signal plane.
func (s *Service) MyStats(ctx context.Context, actor access.Actor) (MyStats, error) {
	out := MyStats{Completed: map[string]int64{}}
	if !actor.Member() {
		return out, fmt.Errorf("%w: account statistics need a signed-in person", ErrInvalid)
	}
	if s.o.Tenure != nil && len(s.o.Kinds.Tiers) > 0 {
		seconds, err := s.o.Tenure.Tenure(ctx, actor.AccountID(), s.o.Kinds.Tiers, s.o.Now())
		if err != nil {
			s.o.Logger.WarnContext(ctx, "discovery: access tenure unavailable", "err", err)
		} else {
			out.AccessSeconds = seconds
		}
	}
	if err := s.o.Pool.QueryRow(ctx, `SELECT count(*), coalesce(sum(likes), 0), coalesce(sum(dislikes), 0) FROM `+s.t.comments+`
 WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL AND moderation = 'approved'`, s.o.Tenant, actor.AccountID()).Scan(
		&out.Comments, &out.CommentLikes, &out.CommentDislikes); err != nil {
		return out, err
	}
	if !s.o.Hub.SignalPlane() {
		return out, nil
	}
	subject := signal.Subject{UserID: actor.AccountID()}
	for _, kind := range s.kindNames() {
		n, err := s.o.Hub.HistoryCount(ctx, subject, signal.HistoryOptions{ContentKind: kind, Status: signal.HistoryCompleted})
		if err != nil {
			s.o.Logger.WarnContext(ctx, "discovery: completed counts unavailable", "err", err)
			out.Completed = map[string]int64{}
			return out, nil
		}
		out.Completed[kind] = n
	}
	return out, nil
}
