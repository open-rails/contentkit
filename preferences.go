package contentkit

import (
	"context"
	"errors"

	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/signal"
)

// PreferenceEventID is the stable signal identity of a subject's current
// preference on one axis: (tenant, canonical reference, subject, axis,
// "current"). A newer revision supersedes the previous one; a re-send carries
// the same revision and converges.
const PreferenceEventID = "current"

// sendPreferences writes one page into the signal plane: one event per
// subject × reference × axis with the row's revision, change time and value
// (0 = neutral / unfavorited). Erased subjects are dropped by the store.
func (r *Runtime) sendPreferences(ctx context.Context, page []content.Preference) error {
	store, err := r.requireStore()
	if err != nil {
		return err
	}
	for len(page) > 0 {
		n := min(len(page), signal.MaxSignalsPerBatch)
		batch := make([]signal.Signal, n)
		for i, p := range page[:n] {
			batch[i] = signal.Signal{ContentRef: p.ContentRef, Subject: signal.Subject{UserID: p.ActorID}, Type: p.Axis,
				EventID: PreferenceEventID, Revision: uint64(p.Revision), OccurredAt: p.UpdatedAt, Value: float64(p.Value)}
		}
		if err := store.RecordSignals(ctx, r.tenant, batch); err != nil {
			return err
		}
		page = page[n:]
	}
	return nil
}

// SyncPreferences sends reactions and favorites changed since the last sync
// into the signal plane (see content.Runtime.SyncPreferences). Schedule it from
// the host's worker; with the signal plane disabled it returns
// ErrSignalPlaneDisabled.
func (r *Runtime) SyncPreferences(ctx context.Context) (content.PreferenceSyncReport, error) {
	if _, err := r.requireStore(); err != nil {
		return content.PreferenceSyncReport{}, err
	}
	return r.Content.SyncPreferences(ctx, r.sendPreferences)
}

// ResyncPreferences re-sends every exportable preference: the periodic repair
// for sink loss. Newer revisions still win.
func (r *Runtime) ResyncPreferences(ctx context.Context) (content.PreferenceSyncReport, error) {
	if _, err := r.requireStore(); err != nil {
		return content.PreferenceSyncReport{}, err
	}
	return r.Content.ResyncPreferences(ctx, r.sendPreferences)
}

// EraseSubjects erases every configured runtime plane: signals, interaction
// data and data retained by moderation/classifier providers. Current approved
// authored content remains under host retention policy
// (content.EraseSubjects). ContentKit-owned reactions, favorites, poll votes
// and unpublished submissions are removed atomically behind the source fence.
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
