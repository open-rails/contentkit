package content

import (
	"context"
	"testing"

	"github.com/open-rails/contentkit/access"
)

func TestReviewC3CountsFollowCanonicalPreference(t *testing.T) {
	rt := newPreferenceRuntime(t)
	mustReact(t, rt, access.Actor{ID: "u1"}, "gallery", localeID(42, "en"), 1)
	if got := countsOf(t, rt, ref("gallery", localeID(42, "en"))); got.Likes != 1 {
		t.Fatalf("locale hydration lost canonical preference count: %+v", got)
	}
}

func TestReviewC3CountsKeepLocalizedCommentThreads(t *testing.T) {
	rt := newPreferenceRuntime(t)
	actor := access.Actor{ID: "u1"}
	mustReact(t, rt, actor, "gallery", localeID(42, "en"), 1)
	mustFavorite(t, rt, actor, "gallery", localeID(42, "ja"), true)
	mustComment(t, rt, actor, "gallery", localeID(42, "en"), createInput{Body: "English"})
	for _, id := range []string{localeID(42, "en"), localeID(42, "ja")} {
		got := countsOf(t, rt, ref("gallery", id))
		wantComments := 0
		if id == localeID(42, "en") {
			wantComments = 1
		}
		if got.Likes != 1 || got.Favorites != 1 || got.CommentCount != wantComments {
			t.Fatalf("counts %s = %+v", id, got)
		}
	}
}

func TestReviewC3FreshSequenceStrictlyExceedsFloor(t *testing.T) {
	rt := newPreferenceRuntime(t)
	floor, err := rt.SeedPreferenceRevisionFloor(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	mustReact(t, rt, access.Actor{ID: "u1"}, "gallery", localeID(42, "en"), 1)
	if _, rev, _ := row(t, rt, rt.store.t.reactions, "u1", cid(42)); rev <= floor {
		t.Fatalf("revision %d failed to exceed seeded floor %d", rev, floor)
	}
}
