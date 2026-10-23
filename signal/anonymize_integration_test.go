package signal

import (
	"reflect"
	"testing"
)

func TestAnonymizationPreservesReaderMetricsAndRejectsOldWrites(t *testing.T) {
	st, conn := freshStore(t)
	ctx := t.Context()
	const tenant = "anonymize"
	old, anonymous := Subject{UserID: "removed-account"}, Subject{AnonKey: "fresh-random-anonymous-key"}
	kept := Subject{UserID: "kept-account"}
	for _, subject := range []Subject{old, kept} {
		if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{view(tenant, cid(1), subject, 1, 10, 10, 10, 50, true), view(tenant, cid(2), subject, 2, 10, 5, 10, 50, false)}); err != nil {
			t.Fatal(err)
		}
	}
	before := metricsByID(t, st, tenant, []string{cid(1), cid(2)}, AllTime())
	for range 2 {
		if err := st.AnonymizeSubject(ctx, tenant, old, anonymous); err != nil {
			t.Fatal(err)
		}
		if after := metricsByID(t, st, tenant, []string{cid(1), cid(2)}, AllTime()); !reflect.DeepEqual(before, after) {
			t.Fatalf("anonymization changed reader metrics: before=%+v after=%+v", before, after)
		}
	}
	tables, err := st.activeSubjectTables(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		if n := countWhere(t, conn, table, "tenant=? AND subject_kind='user' AND subject=?", tenant, old.Key()); n != 0 {
			t.Fatalf("old account remains in %s: %d rows", table, n)
		}
	}
	if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{view(tenant, cid(3), old, 3, 10, 10, 10, 50, true)}); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, conn, "engagement_segments", "tenant=? AND subject_kind='user' AND subject=?", tenant, old.Key()); n != 0 {
		t.Fatal("a removed account resumed writing")
	}
}
