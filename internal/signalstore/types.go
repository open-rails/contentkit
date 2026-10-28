package signalstore

import (
	"fmt"
	"strings"
	"time"

	signal "github.com/open-rails/contentkit/signal"
)

func validateSignal(s signal.Signal, tenant string) error {
	if err := checkRef(tenant, s.ContentRef); err != nil {
		return err
	}
	if err := s.Subject.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(s.Type) == "" {
		return fmt.Errorf("signal: Type is required")
	}
	if strings.TrimSpace(s.EventID) == "" {
		return fmt.Errorf("signal: EventID is required (stable source identity reused by retries)")
	}
	if s.OccurredAt.IsZero() {
		return fmt.Errorf("signal: OccurredAt is required (immutable source time)")
	}
	if err := checkIdentifiers("Subject", s.Subject.Key(), "Type", s.Type, "EventID", s.EventID); err != nil {
		return err
	}
	if err := validateAccessContext(s.Access); err != nil {
		return err
	}
	return checkLen("Resume", s.Resume, signal.MaxResumeBytes)
}

// checkRef validates a reference and pins it to the store call's tenant: a
// reference of another tenant is an error, never silently rewritten.
func checkRef(tenant string, ref signal.ContentRef) error {
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("signal: %w", err)
	}
	if ref.TenantID != tenant {
		return fmt.Errorf("signal: %s belongs to tenant %q, not %q", ref, ref.TenantID, tenant)
	}
	return checkIdentifiers("ContentKind", ref.ContentKind, "ContentID", ref.ContentID, "ContentVersionID", ref.Version())
}

func validateStage(s signal.ExposureStage) error {
	switch s {
	case signal.StageServed, signal.StageRendered, signal.StageVisible:
		return nil
	}
	return fmt.Errorf("exposure: invalid stage %q", s)
}

func validateExposure(e signal.Exposure, tenant string) error {
	if strings.TrimSpace(e.RenderID) == "" {
		return fmt.Errorf("exposure: RenderID is required")
	}
	if err := validateStage(e.Stage); err != nil {
		return err
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("exposure: OccurredAt is required")
	}
	if len(e.Shown) == 0 && !(e.Stage == signal.StageRendered && e.Surface == signal.SurfaceSearch && strings.TrimSpace(e.QueryID) != "" && strings.TrimSpace(e.SearchQuery) != "") {
		return fmt.Errorf("exposure: at least one placement is required")
	}
	if len(e.Shown) > signal.MaxShownPerExposure {
		return &signal.LimitError{Field: "Shown", Limit: signal.MaxShownPerExposure, Got: len(e.Shown)}
	}
	seen := map[uint32]struct{}{}
	for i, p := range e.Shown {
		if err := checkRef(tenant, p.ContentRef); err != nil {
			return fmt.Errorf("exposure: placement %d: %w", i, err)
		}
		if err := validateAccessContext(p.Access); err != nil {
			return err
		}
		if p.Position == 0 {
			return fmt.Errorf("exposure: placement %d: Position is required (1-based)", i)
		}
		if _, dup := seen[p.Position]; dup {
			return fmt.Errorf("exposure: duplicate position %d", p.Position)
		}
		seen[p.Position] = struct{}{}
	}
	if e.Subject != (signal.Subject{}) {
		if err := e.Subject.Validate(); err != nil {
			return err
		}
	}
	return checkIdentifiers("RenderID", e.RenderID, "QueryID", e.QueryID, "Surface", e.Surface,
		"Ranker", e.Ranker, "Language", e.Language, "Subject", e.Subject.Key())
}

// LastDays returns the n most recent UTC calendar days including the day that
// contains now: [midnight(now) - (n-1) days, midnight(now) + 1 day). The window
// moves only at UTC midnight, so String is a stable cache key within a day.
// n must be positive (Validate reports otherwise).
func LastDays(n int, now time.Time) signal.Window {
	today := utcDay(now)
	return signal.Window{From: today.AddDate(0, 0, 1-n), To: today.AddDate(0, 0, 1)}
}

// Between returns the whole UTC days [from, to); both must be UTC midnights.
func Between(from, to time.Time) signal.Window { return signal.Window{From: from, To: to} }

// AllTime returns the unbounded window.
func AllTime() signal.Window { return signal.Window{} }

// predicate renders " AND column >= ? AND column < ?" for the bounded sides.
func windowPredicate(w signal.Window, column string) (string, []any) {
	var (
		sb   strings.Builder
		args []any
	)
	if !w.From.IsZero() {
		sb.WriteString(" AND " + column + " >= ?")
		args = append(args, w.From.UTC())
	}
	if !w.To.IsZero() {
		sb.WriteString(" AND " + column + " < ?")
		args = append(args, w.To.UTC())
	}
	return sb.String(), args
}

// dayPredicate renders the window over a Date column.
func windowDayPredicate(w signal.Window, column string) (string, []any) {
	var (
		sb   strings.Builder
		args []any
	)
	if !w.From.IsZero() {
		sb.WriteString(" AND " + column + " >= toDate(?)")
		args = append(args, w.From.UTC().Format(time.DateOnly))
	}
	if !w.To.IsZero() {
		sb.WriteString(" AND " + column + " < toDate(?)")
		args = append(args, w.To.UTC().Format(time.DateOnly))
	}
	return sb.String(), args
}

func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
