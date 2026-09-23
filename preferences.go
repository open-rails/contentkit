package contentkit

import (
	"context"
	"errors"

	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/signal"
)

// PreferenceEventID is the stable signal identity of a subject's current
// preference on one axis: (tenant, canonical reference, subject, axis,
// "current"). Every delivery of a newer snapshot supersedes the previous one
// by revision; a replay carries the same revision and converges.
const PreferenceEventID = "current"

// preferenceSink writes preference snapshots into the signal plane: one
// `signal` event per subject × reference × axis with the persisted revision,
// occurrence time and current value. Fenced subjects are reported erased;
// a failed insert leaves the page pending.
type preferenceSink struct {
	store  *signal.Store
	tenant string
}

func (s preferenceSink) DeliverPreferences(ctx context.Context, snaps []content.PreferenceSnapshot) ([]content.PreferenceDisposition, error) {
	out := make([]content.PreferenceDisposition, len(snaps))
	subjects := make([]signal.Subject, 0, len(snaps))
	for _, sn := range snaps {
		subjects = append(subjects, signal.Subject{UserID: sn.ActorID})
	}
	erased, err := s.store.ErasedSubjects(ctx, s.tenant, subjects)
	if err != nil {
		return nil, err
	}
	var batch []signal.Signal
	var idx []int
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := s.store.RecordSignals(ctx, s.tenant, batch); err != nil {
			return err
		}
		for _, i := range idx {
			out[i] = content.PreferenceAccepted
		}
		batch, idx = batch[:0], idx[:0]
		return nil
	}
	for i, sn := range snaps {
		if erased[signal.Subject{UserID: sn.ActorID}] {
			out[i] = content.PreferenceSubjectErased
			continue
		}
		batch = append(batch, signal.Signal{
			ContentRef: sn.Ref(),
			Subject:    signal.Subject{UserID: sn.ActorID},
			Type:       sn.Axis,
			EventID:    PreferenceEventID,
			Revision:   uint64(sn.Revision),
			OccurredAt: sn.OccurredAt,
			Value:      float64(sn.Value),
		})
		idx = append(idx, i)
		if len(batch) == signal.MaxSignalsPerBatch {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Runtime) preferenceSink() (content.PreferenceSink, error) {
	store, err := r.requireStore()
	if err != nil {
		return nil, err
	}
	return preferenceSink{store: store, tenant: r.tenant}, nil
}

// DeliverPreferences runs one bounded sweep of the tenant's pending preference
// snapshots into the signal plane (see content.Runtime.DeliverPreferences).
// Schedule it from the host's worker; with the signal plane disabled it
// returns ErrSignalPlaneDisabled and every row stays pending.
func (r *Runtime) DeliverPreferences(ctx context.Context, after content.PreferenceKey, pageSize, maxRows int) (content.PreferenceDelivery, error) {
	sink, err := r.preferenceSink()
	if err != nil {
		return content.PreferenceDelivery{}, err
	}
	return r.Content.DeliverPreferences(ctx, sink, after, pageSize, maxRows)
}

// ReplayPreferences replays every snapshot from after into the signal plane
// (acknowledged rows and zeros included): the repair for sink loss, resumable
// from the returned Next.
func (r *Runtime) ReplayPreferences(ctx context.Context, after content.PreferenceKey, pageSize, maxRows int) (content.PreferenceDelivery, error) {
	sink, err := r.preferenceSink()
	if err != nil {
		return content.PreferenceDelivery{}, err
	}
	return r.Content.ReplayPreferences(ctx, sink, after, pageSize, maxRows)
}

// EraseSubjects erases every configured runtime plane: signals, preference
// obligations, interaction data and data retained by moderation/classifier
// providers. Current approved authored content remains under host retention
// policy (content.EraseSubjects). ContentKit-owned reactions, favorites, poll
// votes, preference obligations and unpublished submissions are removed
// atomically behind the source fence.
// EmbeddedHub.EraseSubjects is the explicit analytics-only lower-level API.
//
// AuthKit ACK means durable acceptance by the host's deletion ledger, not this
// downstream completion. Retry while error != nil or !report.Complete(). A
// disabled signal plane is intentionally absent. Remaining may include pending
// plane markers content_plane or signal_plane when a
// plane could not complete; these are not estimates of retained provider rows.
func (r *Runtime) EraseSubjects(ctx context.Context, subjects []signal.Subject) (signal.ErasureReport, error) {
	invalid := signal.ErasureReport{Remaining: map[string]uint64{"invalid_subjects": 1}}
	if len(subjects) > signal.MaxErasureSubjects {
		return invalid, &signal.LimitError{Field: "subjects per erasure", Limit: signal.MaxErasureSubjects, Got: len(subjects)}
	}
	for _, s := range subjects {
		if err := s.Validate(); err != nil {
			return invalid, err
		}
	}
	report, signalErr := r.EmbeddedHub.EraseSubjects(ctx, subjects)
	if errors.Is(signalErr, ErrSignalPlaneDisabled) {
		signalErr = nil
	}
	if report.Remaining == nil {
		report.Remaining = map[string]uint64{}
	}
	var actors []string
	for _, s := range subjects {
		if s.Kind() == signal.SubjectKindUser {
			actors = append(actors, s.Key())
		}
	}
	contentErr := r.Content.EraseSubjects(ctx, actors)
	if signalErr != nil {
		report.Remaining["signal_plane"] = 1
	}
	if contentErr != nil {
		report.Remaining["content_plane"] = 1
	}
	return report, errors.Join(signalErr, contentErr)
}
