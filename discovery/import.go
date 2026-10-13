package discovery

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/popularity"
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
	DurationS  uint32
	Progress   uint32
	Resume     string
}

// SignalPlane reports whether signals are recorded.
func (s *Service) SignalPlane() bool { return s.recs != nil }

// ImportSignals records imported events in the signal plane, without a
// visibility check: a view on a version also counts for its content.
// ErrNoSignals without a signal plane.
func (s *Service) ImportSignals(ctx context.Context, in []ImportSignal) error {
	if s.recs == nil {
		return ErrNoSignals
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
		if err := identifiers(x.EventID, x.Resume, x.Subject.Key()); err != nil || x.EventID == "" || x.OccurredAt.IsZero() {
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
		facts, versioned := t.versions[x.VersionID]
		if x.VersionID != "" && !versioned {
			return fmt.Errorf("%w: version %s of content %s", ErrNotFound, x.VersionID, x.ContentID)
		}
		ref := contentref.New(s.o.Tenant, t.kind, x.ContentID)
		sig := signal.Signal{ContentRef: ref, Subject: x.Subject, Type: x.Type, EventID: x.EventID, Revision: x.Revision, OccurredAt: x.OccurredAt.UTC(),
			DurationS: x.DurationS, Resume: x.Resume}
		switch x.Type {
		case TypeView:
			sig.Progress, sig.ProgressMax = x.Progress, progressMax(t.presentation, facts)
			if sig.ProgressMax > 0 {
				sig.Progress = min(sig.Progress, sig.ProgressMax)
			}
			sig.Score, sig.Completed = popularity.SessionScore(sig.Progress, sig.ProgressMax, x.DurationS, secondsPerUnit(t.presentation))
		case TypeClick:
		default:
			return fmt.Errorf("%w: signal type %q is view or click", ErrInvalid, x.Type)
		}
		out = append(out, sig)
		if x.Type == TypeView && x.VersionID != "" {
			v := sig
			v.ContentRef = ref.WithVersion(x.VersionID)
			out = append(out, v)
		}
	}
	return s.o.Hub.RecordSignals(ctx, out)
}
