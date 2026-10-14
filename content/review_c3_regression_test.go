package content

import (
	"testing"

	"github.com/open-rails/contentkit/access"
)

func TestReviewC3FreshSequenceStrictlyExceedsFloor(t *testing.T) {
	rt := newPreferenceRuntime(t)
	floor := int64(1)
	if err := raiseFloor(rt, floor); err != nil {
		t.Fatal(err)
	}
	mustReact(t, rt, access.Actor{ID: "u1"}, "gallery", localeID(42, "en"), 1)
	if _, rev, _ := row(t, rt, rt.store.t.reactions, "u1", cid(42)); rev <= floor {
		t.Fatalf("revision %d failed to exceed seeded floor %d", rev, floor)
	}
}
