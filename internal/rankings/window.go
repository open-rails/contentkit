package rankings

import (
	"fmt"
	"strings"
	"time"
)

// Featured windows: rolling, including today so far, recomputed after each
// hourly rebuild.
const (
	// Week is the last 7 days: trending, per live day.
	Week = "week"
	// Days30 and Days90 are the last 30 and 90 days.
	Days30 = "30d"
	Days90 = "90d"
	// Year is the calendar year so far.
	Year = "year"
	// All is every day.
	All = "all"
)

// Featured are the featured windows, in display order.
var Featured = []string{Week, Days30, Days90, Year, All}

// DefaultWindow is the window a popular list without one ranks by.
const DefaultWindow = Week

// Window is a ranking's span of UTC days [From, Until).
type Window struct {
	// Name is the window as requested, in its canonical form: a featured
	// window, a calendar month ("month:2026-10") or year ("year:2025"), or
	// a range of days ("2026-12-01..2026-12-15", last day included).
	Name        string
	From, Until time.Time
	// Featured windows are precomputed; any other is a range.
	Featured bool
	// Trending ranks per live day.
	Trending bool
}

// epoch is the first day of All.
var epoch = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// ParseWindow reads a window at now; "" is DefaultWindow. A range ends by
// tomorrow at the latest: today counts so far.
func ParseWindow(s string, now time.Time) (Window, error) {
	now = now.UTC()
	today := day(now)
	tomorrow := today.AddDate(0, 0, 1)
	invalid := func() (Window, error) {
		return Window{}, fmt.Errorf("%w: window %q is week, 30d, 90d, year, all, month:YYYY-MM, year:YYYY or YYYY-MM-DD..YYYY-MM-DD", ErrInvalid, s)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		s = DefaultWindow
	}
	w := Window{Name: s, Until: tomorrow, Featured: true}
	switch s {
	case Week:
		w.From, w.Trending = today.AddDate(0, 0, -6), true
	case Days30:
		w.From = today.AddDate(0, 0, -29)
	case Days90:
		w.From = today.AddDate(0, 0, -89)
	case Year:
		w.From = time.Date(today.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	case All:
		w.From = epoch
	default:
		w.Featured = false
		switch {
		case strings.HasPrefix(s, "month:"):
			m, err := time.Parse("2006-01", strings.TrimPrefix(s, "month:"))
			if err != nil {
				return invalid()
			}
			w.From, w.Until = m, m.AddDate(0, 1, 0)
		case strings.HasPrefix(s, "year:"):
			y, err := time.Parse("2006", strings.TrimPrefix(s, "year:"))
			if err != nil {
				return invalid()
			}
			w.From, w.Until = y, y.AddDate(1, 0, 0)
		default:
			a, b, ok := strings.Cut(s, "..")
			from, err1 := time.Parse(time.DateOnly, a)
			last, err2 := time.Parse(time.DateOnly, b)
			if !ok || err1 != nil || err2 != nil || last.Before(from) {
				return invalid()
			}
			w.From, w.Until = from, last.AddDate(0, 0, 1)
		}
		if !w.From.Before(tomorrow) || w.From.Before(epoch) {
			return Window{}, fmt.Errorf("%w: window %q starts after today", ErrInvalid, s)
		}
		w.Until = earlier(w.Until, tomorrow)
	}
	return w, nil
}

// Range is the window of days [from, until).
func Range(from, until time.Time) Window {
	from, until = day(from), day(until)
	return Window{Name: from.Format(time.DateOnly) + ".." + until.AddDate(0, 0, -1).Format(time.DateOnly), From: from, Until: until}
}

// closedBy reports whether w's last day is final: its post-midnight rebuild
// has run, so its ranking never changes for one outcome_values version.
func (w Window) closedBy(final time.Time) bool {
	return !w.Featured && !final.IsZero() && !w.Until.After(final.AddDate(0, 0, 1))
}

func day(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
