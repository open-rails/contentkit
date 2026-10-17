package discovery

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
)

// ImportSignal is a recorded event as the bulk import replays it: its
// subject given, scored as RecordSignals scores a client's.
type ImportSignal struct {
	ContentID  string
	VersionID  string
	Subject    signal.Subject
	Type       string
	EventID    string
	Revision   uint64
	OccurredAt time.Time
}

// SignalPlane reports whether signals are recorded now.
func (s *Service) SignalPlane() bool { return s.o.Hub.SignalPlane() }

// ImportSignals records imported events in the signal plane, without a
// visibility check: a view on a version also counts for its content.
// ErrNoSignals without a signal plane (ErrSignalsDown while it is down).
func (s *Service) ImportSignals(ctx context.Context, in []ImportSignal) error {
	if err := s.signals(); err != nil {
		return err
	}
	if len(in) == 0 {
		return nil
	}
	ids := make([]string, 0, len(in))
	for _, x := range in {
		if err := contentref.ValidateID(x.ContentID); err != nil {
			return fmt.Errorf("%w: content_id: %w", ErrInvalid, err)
		}
		if err := x.Subject.Validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		if err := identifiers(x.EventID, x.Subject.Key()); err != nil || x.EventID == "" || x.OccurredAt.IsZero() {
			return fmt.Errorf("%w: a signal needs an event_id and occurred_at, and identifiers of at most %d bytes", ErrInvalid, signal.MaxIdentifierBytes)
		}
		ids = append(ids, x.ContentID)
	}
	targets, err := s.targets(ctx, ids)
	if err != nil {
		return err
	}
	out := make([]signal.Signal, 0, len(in))
	for _, x := range in {
		t, ok := targets[x.ContentID]
		if !ok {
			return fmt.Errorf("%w: content %s", ErrNotFound, x.ContentID)
		}
		_, versioned := t.versions[x.VersionID]
		if x.VersionID != "" && !versioned {
			return fmt.Errorf("%w: version %s of content %s", ErrNotFound, x.VersionID, x.ContentID)
		}
		ref := contentref.New(s.o.Tenant, t.kind, x.ContentID)
		if x.Type != TypeClick {
			return fmt.Errorf("%w: imported signals are explicit clicks; legacy resume/progress is preserved separately", ErrInvalid)
		}
		out = append(out, signal.Signal{ContentRef: ref, Subject: x.Subject, Type: TypeClick, EventID: x.EventID, Revision: x.Revision, OccurredAt: x.OccurredAt.UTC()})

	}
	return s.o.Hub.RecordSignals(ctx, out)
}
