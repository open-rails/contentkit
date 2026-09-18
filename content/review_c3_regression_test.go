package content

import (
	"context"
	"testing"
)

func TestReviewC3BoundedDeliveryFairness(t *testing.T) {
	rt := newPreferenceRuntime(t)
	for _, actor := range []string{"u1", "u2"} {
		mustReact(t, rt, Actor{ID: actor}, "gallery", "42:en", 1)
	}
	sink := &deliverySink{verdicts: map[string]PreferenceDisposition{"u1": PreferenceRetry, "u2": PreferenceAccepted}}
	for i := 0; i < 3; i++ {
		if _, err := rt.DeliverPreferences(context.Background(), sink, 1, 1); err != nil {
			t.Fatal(err)
		}
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
}
