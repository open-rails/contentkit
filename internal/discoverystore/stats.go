package discoverystore

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/open-rails/contentkit/access"
	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/readingstore"
	"github.com/open-rails/contentkit/internal/subject"
)

var statsRoutes = []httpapi.Route[handler]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/stats/active", Resource: "stats", Auth: httpapi.Staff, Perm: string(access.ContentViewRestricted),
		Doc:       "Distinct people active on the site for every UTC day of the period, newest first, by access (anonymous, account or the tiers an account's token claimed), from the analytics job's daily rollups; today is as of its last hourly rebuild.",
		Query:     []httpapi.Param{httpapi.Int("days", "the last days, today included: 10 by default, at most 90")},
		Responses: []httpapi.Reply{httpapi.OK(discovery.ActiveDays{})}, Errors: signals}, Serve: httpapi.H(handler.active)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/me/stats", Resource: "stats", Auth: httpapi.User,
		Doc:       "The signed-in reader's account statistics: the contents they finished per kind, their comments and the reactions those received, and elapsed access seconds per tier when billing supplies its history.",
		Responses: []httpapi.Reply{httpapi.OK(discovery.MyStats{})}}, Serve: httpapi.H(handler.myStats)},
}

func (h handler) active(w http.ResponseWriter, r *http.Request) {
	days, err := intParam(r, "days")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.s.Active(r.Context(), days)
	h.reply(w, r, out, err)
}

func (h handler) myStats(w http.ResponseWriter, r *http.Request) {
	out, err := h.s.MyStats(r.Context(), gate.Actor(r.Context()))
	h.reply(w, r, out, err)
}

// Active reads the site's active people per day of the last days (10 by
// default, at most 90) from audience_daily.
func (s *Service) Active(ctx context.Context, days int) (discovery.ActiveDays, error) {
	out := discovery.ActiveDays{Days: []discovery.ActiveDay{}}
	switch {
	case days == 0:
		days = 10
	case days < 0:
		return out, fmt.Errorf("%w: days must be positive", ErrInvalid)
	case days > 90:
		return out, fmt.Errorf("%w: days is at most 90", ErrInvalid)
	}
	if err := s.signals(); err != nil {
		return out, err
	}
	store := s.o.Hub.Signals()
	if store == nil {
		return out, ErrSignalsDown
	}
	today := utcDay(s.o.Now())
	from := today.AddDate(0, 0, 1-days)
	got, err := store.ActiveDays(ctx, s.o.Tenant, from, today.AddDate(0, 0, 1))
	if err != nil {
		return out, err
	}
	for i := range days {
		date := today.AddDate(0, 0, -i)
		d := discovery.ActiveDay{Date: date.Format(time.DateOnly), ByAccess: map[string]uint64{}}
		for access, n := range got[date] {
			d.ByAccess[access] = n
			d.People += n
		}
		out.Days = append(out.Days, d)
	}
	return out, nil
}

// MyStats reads a person's account statistics.
func (s *Service) MyStats(ctx context.Context, actor access.Actor) (discovery.MyStats, error) {
	out := discovery.MyStats{Completed: map[string]int64{}}
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
 WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL AND moderation = 'approved' AND NOT author_hidden`, s.o.Tenant, actor.AccountID()).Scan(
		&out.Comments, &out.CommentLikes, &out.CommentDislikes); err != nil {
		return out, err
	}
	for _, kind := range s.kindNames() {
		out.Completed[kind] = 0
	}
	who, ok := subject.Account(actor.AccountID())
	if !ok {
		return out, nil
	}
	rows, err := s.o.Pool.Query(ctx, `SELECT c.kind, count(*) FROM `+s.t.reading+` r JOIN `+s.t.items+` c ON c.tenant_id = $1 AND c.id = r.content_id::text
 WHERE r.app_id = $2 AND r.subject = $3 AND r.best_outcome = $4 GROUP BY c.kind`, s.o.Tenant, s.app(), who, readingstore.OutcomeComplete)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var n int64
		if err := rows.Scan(&kind, &n); err != nil {
			return out, err
		}
		if _, ok := out.Completed[kind]; ok {
			out.Completed[kind] = n
		}
	}
	return out, rows.Err()
}

func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
