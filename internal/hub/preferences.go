package hub

import (
	"context"

	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/signal"
)

// PreferenceEventID is the stable signal identity of a subject's current
// preference on one axis: (tenant, canonical reference, subject, axis,
// "current"). A newer revision supersedes the previous one; a re-send carries
// the same revision and converges.
const PreferenceEventID = "current"

// SendPreferences writes one page into the signal plane: one event per
// subject × reference × axis with the row's revision, change time and value
// (0 = neutral / unfavorited). Erased subjects are dropped by the store. It
// is content's PreferenceSender.
func (h *Hub) SendPreferences(ctx context.Context, page []content.Preference) error {
	store, err := h.requireStore()
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
		if err := store.RecordSignals(ctx, h.tenant, batch); err != nil {
			return err
		}
		page = page[n:]
	}
	return nil
}

// SignalPlane reports whether the hub's signal plane is up.
func (h *Hub) SignalPlane() bool { return h.store.Load() != nil }
