package signal

import (
	"fmt"
	"reflect"
	"time"

	"github.com/open-rails/contentkit/internal/signaltest"
	"testing"
)

func TestDailyActivityCohortsDeduplicateAndUseTrustedDayPriority(t *testing.T) {
	env := signaltest.FromEnv(t)
	database := fmt.Sprintf("contentkit_activity_test_%d", time.Now().UnixNano())
	admin := env.Open(t, "")
	// Never drop a pre-existing database: a collision makes CREATE fail.
	if err := admin.Exec(t.Context(), "CREATE DATABASE "+database+env.OnCluster()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { env.Drop(t, admin, database) })
	conn := env.Open(t, database)
	env.Apply(t, conn)
	st, err := NewStore(conn, database)
	if err != nil {
		t.Fatal(err)
	}
	ctx, tenant := t.Context(), "cohort-test"
	member, guest := Subject{UserID: "member"}, Subject{AnonKey: "guest"}
	observations := []PopulationObservation{
		{ID: "before-upgrade", ContentKind: "gallery", Subject: member, ObservedAt: at(1, 9), EligibilityKnown: true},
		{ID: "after-upgrade", ContentKind: "gallery", Subject: member, ObservedAt: at(1, 12), EligibilityKnown: true, EligibleTiers: []string{"supporter"}},
		{ID: "next-day", ContentKind: "gallery", Subject: member, ObservedAt: at(2, 9), EligibilityKnown: true},
		// Unverified eligibility must not promote a reader into a paid group.
		{ID: "unverified", ContentKind: "gallery", Subject: member, ObservedAt: at(2, 12), EligibleTiers: []string{"supporter"}},
	}
	if err := st.RecordPopulation(ctx, tenant, observations); err != nil {
		t.Fatal(err)
	}
	if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{
		view(tenant, cid(1), member, 1, 10, 2, 10, 0, false),
		view(tenant, cid(2), member, 1, 13, 2, 10, 0, false),
		view(tenant, cid(1), member, 2, 10, 2, 10, 0, false),
		view(tenant, cid(1), guest, 1, 10, 2, 10, 0, false),
	}); err != nil {
		t.Fatal(err)
	}
	cohorts := []ActivityCohort{
		{ID: "supporter", Identity: ActivityAccount, AnyTier: []string{"supporter"}},
		{ID: "member", Identity: ActivityAccount},
		{ID: "guest", Identity: ActivityAnonymous},
	}
	got, err := st.Activity(ctx, tenant, "gallery", Window{}, cohorts...)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Readers != 1 || !reflect.DeepEqual(got[0].Cohorts, map[string]uint64{"member": 1}) ||
		got[1].Readers != 2 || !reflect.DeepEqual(got[1].Cohorts, map[string]uint64{"supporter": 1, "guest": 1}) {
		t.Fatalf("daily cohorts = %+v", got)
	}
	partial, err := st.Activity(ctx, tenant, "gallery", Window{}, cohorts[0])
	if err != nil || len(partial) != 2 || partial[1].Readers != 2 || partial[1].Cohorts["supporter"] != 1 || len(partial[0].Cohorts) != 0 {
		t.Fatalf("unmatched readers must stay in the daily total: %+v, %v", partial, err)
	}
	// A host that defines no cohorts receives totals, without invented entities.
	totals, err := st.Activity(ctx, tenant, "gallery", Window{})
	if err != nil || len(totals) != 2 || totals[1].Readers != 2 || len(totals[1].Cohorts) != 0 {
		t.Fatalf("totals = %+v, %v", totals, err)
	}
}
