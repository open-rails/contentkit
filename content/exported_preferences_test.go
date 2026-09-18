package content

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestReconcileExportedPreferencesIsBoundedCanonicalAndIdempotent(t *testing.T) {
	ctx := context.Background()
	rt := newPreferenceRuntime(t)
	if _, err := rt.SeedPreferenceRevisionFloor(ctx, 1000000); err != nil {
		t.Fatal(err)
	}
	existing := mustReact(t, rt, Actor{ID: "current"}, "gallery", "42:en", 1)
	erased := prefKey("erased", "gallery", "42:en", PreferenceAxisReaction)
	if err := rt.EraseSubjects(ctx, []string{erased.ActorID}); err != nil {
		t.Fatal(err)
	}
	missing := prefKey("gone", "gallery", "42:en", PreferenceAxisFavorite)
	version := missing
	version.ContentVersionID = "explicit"
	keys := []PreferenceKey{existing.PreferenceKey, missing, missing, version, erased}
	n, err := rt.ReconcileExportedPreferences(ctx, keys)
	if err != nil || n != 2 {
		t.Fatalf("inserted %d %v", n, err)
	}
	before := snapshotRows(t, rt)
	for _, s := range before {
		if s.ActorID == "gone" && (s.Value != 0 || s.ContentID != "42" || s.Revision <= 1000000) {
			t.Fatalf("bad canonical tombstone %+v", s)
		}
		if s.ActorID == "erased" {
			t.Fatal("erased subject recreated")
		}
	}
	if got := snapshotRow(t, rt, existing.PreferenceKey); got.Revision != existing.Revision || got.Value != 1 || !got.OccurredAt.Equal(existing.OccurredAt) {
		t.Fatalf("existing source changed: %+v", got)
	}
	n, err = rt.ReconcileExportedPreferences(ctx, keys)
	if err != nil || n != 0 || !reflect.DeepEqual(before, snapshotRows(t, rt)) {
		t.Fatalf("retry changed source: %d %v", n, err)
	}
	foreign := missing
	foreign.TenantID = "other"
	if _, err := rt.ReconcileExportedPreferences(ctx, []PreferenceKey{missing, foreign}); !errors.Is(err, ErrTenant) {
		t.Fatalf("foreign key accepted: %v", err)
	}
	if _, err := rt.ReconcileExportedPreferences(ctx, make([]PreferenceKey, MaxExportedPreferencesPerBatch+1)); err == nil {
		t.Fatal("unbounded batch accepted")
	}
}
