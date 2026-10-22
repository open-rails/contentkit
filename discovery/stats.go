package discovery

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/signal"
)

// Activity is a kind's measured activity for every UTC day of the period,
// newest first; days without activity are zero.
type Activity struct {
	Days []ActivityDay `json:"days"`
	// Cohorts lists application-defined category IDs in matching priority order.
	Cohorts []string `json:"cohorts"`
}

// ActivityDay is one day's distinct people who read or played the kind, by
// application-defined cohort. Readers counts all, including unmatched people.
type ActivityDay struct {
	Date     string            `json:"date"`
	Readers  uint64            `json:"readers"`
	Cohorts  map[string]uint64 `json:"cohorts"`
	ActiveMS uint64            `json:"active_ms"`
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

var statsRoutes = []httpapi.Route[handler]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/stats/activity", Resource: "stats", Auth: httpapi.Staff, Perm: string(access.ContentViewRestricted),
		Doc:       "A content kind's measured readership for every UTC day of the period, newest first: distinct people who read or played it (a visit without reading never counts), split into application-defined cohorts, with their focused active time, from the signal plane.",
		Query:     []httpapi.Param{httpapi.Text("kind", "the content kind; required"), httpapi.Int("days", "the last days, today included: 10 by default, at most 90")},
		Responses: []httpapi.Reply{httpapi.OK(Activity{})}, Errors: signals}, Serve: httpapi.H(handler.activity)},
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

func (h handler) myStats(w http.ResponseWriter, r *http.Request) {
	out, err := h.s.MyStats(r.Context(), gate.Actor(r.Context()))
	h.reply(w, r, out, err)
}

// Activity reads a kind's measured readership per day of the last days (10 by default, at
// most 90).
func (s *Service) Activity(ctx context.Context, kind string, days int) (Activity, error) {
	out := Activity{Days: []ActivityDay{}, Cohorts: []string{}}
	for _, cohort := range s.o.ActivityCohorts {
		out.Cohorts = append(out.Cohorts, cohort.ID)
	}
	if _, ok := s.o.Kinds.Kinds[kind]; !ok {
		return out, fmt.Errorf("%w: kind %q is not configured", ErrInvalid, kind)
	}
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
	now := s.o.Now()
	got, err := s.o.Hub.Activity(ctx, kind, signal.LastDays(days, now), s.o.ActivityCohorts...)
	if err != nil {
		return out, err
	}
	byDay := make(map[string]signal.DayActivity, len(got))
	for _, d := range got {
		byDay[d.Day.Format(time.DateOnly)] = d
	}
	today := now.UTC()
	for i := range days {
		date := today.AddDate(0, 0, -i).Format(time.DateOnly)
		d := byDay[date]
		counts := make(map[string]uint64, len(out.Cohorts))
		for _, id := range out.Cohorts {
			counts[id] = d.Cohorts[id]
		}
		out.Days = append(out.Days, ActivityDay{Date: date, Readers: d.Readers, Cohorts: counts, ActiveMS: d.ActiveMS})
	}
	return out, nil
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
		n, err := s.o.Hub.CompletedCount(ctx, subject, kind)
		if err != nil {
			s.o.Logger.WarnContext(ctx, "discovery: completed counts unavailable", "err", err)
			out.Completed = map[string]int64{}
			return out, nil
		}
		out.Completed[kind] = n
	}
	return out, nil
}
