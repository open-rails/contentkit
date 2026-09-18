package content

import (
	"context"
	"errors"
	"testing"
)

func TestReviewC3BoundedDeliveryFairness(t *testing.T) {
	rt := newPreferenceRuntime(t)
	for _, actor := range []string{"u1", "u2"} {
		mustReact(t, rt, Actor{ID: actor}, "gallery", "42:en", 1)
	}
	sink := &deliverySink{verdicts: map[string]PreferenceDisposition{"u1": PreferenceRetry, "u2": PreferenceAccepted}}
	var after PreferenceKey
	for i := 0; i < 3; i++ {
		report, err := rt.DeliverPreferences(context.Background(), sink, after, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		after = report.Next
	}
	if len(pending(t, rt)) != 1 {
		t.Fatal("bounded delivery repeatedly selects failing u1 and never reaches u2")
	}
}

func TestReviewC3MigrationActorChangesIP(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	for _, r := range []struct {
		id, ip string
		value  int
	}{{"42:en", "192.0.2.1", 1}, {"42:ja", "192.0.2.2", -1}} {
		_, err := rt.store.pool.Exec(ctx, `INSERT INTO `+rt.store.t.reactions+` (`+keyCols+`,user_id,ip,value) VALUES ($1,'gallery',$2,'','u1',$3,$4)`, rt.tenant, r.id, r.ip, r.value)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rt.MigratePreferences(ctx, PreferenceMigrationOptions{}); err != nil {
		t.Fatalf("one authenticated user on two IPs must collapse to one work preference: %v", err)
	}
	if got := snapshotRow(t, rt, prefKey("u1", "gallery", "42", PreferenceAxisReaction)); got.Value != -1 {
		t.Fatalf("collapsed snapshot = %+v", got)
	}
	if got := countsOf(t, rt, ref("gallery", "42")); got.Likes != 0 || got.Dislikes != 1 {
		t.Fatalf("collapsed counts = %+v", got)
	}
	var originals int
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.preferenceArchive+` WHERE actor_id='u1' AND ip IN ('192.0.2.1','192.0.2.2')`).Scan(&originals); err != nil || originals != 2 {
		t.Fatalf("original IP provenance lost: %d %v", originals, err)
	}
}

func TestReviewC3CountsFollowCanonicalPreference(t *testing.T) {
	rt := newPreferenceRuntime(t)
	mustReact(t, rt, Actor{ID: "u1"}, "gallery", "42:en", 1)
	if got := countsOf(t, rt, ref("gallery", "42:en")); got.Likes != 1 {
		t.Fatalf("locale hydration lost canonical preference count: %+v", got)
	}
}

func TestReviewC3ErasurePurgesPreferenceArchive(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	mustReact(t, rt, Actor{ID: "erased", IP: "192.0.2.1"}, "gallery", "42:en", 1)
	if _, err := rt.MigratePreferences(ctx, PreferenceMigrationOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.store.pool.Exec(ctx, `INSERT INTO `+rt.store.t.preferenceArchive+` (tenant_id,axis,actor_id,ip,content_kind,content_id,content_version_id,value,source_at) SELECT 'other_tenant',axis,actor_id,ip,content_kind,content_id,content_version_id,value,source_at FROM `+rt.store.t.preferenceArchive); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.PurgePreferenceSubjects(ctx, []string{"erased"}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.preferenceArchive+` WHERE tenant_id=$1 AND actor_id='erased'`, rt.tenant).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("erased actor still has %d archived preference rows including actor ID and IP", n)
	}
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.preferenceArchive+` WHERE tenant_id='other_tenant' AND actor_id='erased'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("erasure crossed tenant boundary: %d %v", n, err)
	}
}

func TestReviewC3DeliveryResumesAfterFailedPage(t *testing.T) {
	rt := newPreferenceRuntime(t)
	ctx := context.Background()
	for _, actor := range []string{"u1", "u2"} {
		mustReact(t, rt, Actor{ID: actor}, "gallery", "42:en", 1)
	}
	failed, err := rt.DeliverPreferences(ctx, &deliverySink{fail: errors.New("bad page")}, PreferenceKey{}, 1, 1)
	if err == nil || failed.Next.ActorID != "u1" {
		t.Fatalf("failed page lost resume cursor: %+v %v", failed, err)
	}
	sink := &deliverySink{verdicts: map[string]PreferenceDisposition{"u2": PreferenceAccepted}}
	if _, err := rt.DeliverPreferences(ctx, sink, failed.Next, 1, 1); err != nil {
		t.Fatal(err)
	}
	if got := pending(t, rt); len(got) != 1 || got[0].ActorID != "u1" {
		t.Fatalf("failed page starved unrelated work: %+v", got)
	}
}

func TestReviewC3CountsKeepLocalizedCommentThreads(t *testing.T) {
	rt := newPreferenceRuntime(t)
	actor := Actor{ID: "u1"}
	mustReact(t, rt, actor, "gallery", "42:en", 1)
	mustFavorite(t, rt, actor, "gallery", "42:ja", true)
	mustComment(t, rt, actor, "gallery", "42:en", createInput{Body: "English"})
	for _, id := range []string{"42:en", "42:ja"} {
		got := countsOf(t, rt, ref("gallery", id))
		wantComments := 0
		if id == "42:en" {
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
	snap := mustReact(t, rt, Actor{ID: "u1"}, "gallery", "42:en", 1)
	if snap.Revision <= floor {
		t.Fatalf("revision %d failed to exceed seeded floor %d", snap.Revision, floor)
	}
}
