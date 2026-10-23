package signal

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestAnonymizationPreservesReaderMetricsAndRejectsOldWrites(t *testing.T) {
	st, conn := freshStore(t)
	if cluster := os.Getenv("CONTENTKIT_TEST_CH_CLUSTER"); cluster != "" {
		var err error
		st, err = NewStore(conn, testDB, StoreOptions{Cluster: cluster})
		if err != nil {
			t.Fatal(err)
		}
	}
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
	want := make(map[string]ContentMetrics, len(before))
	for id, metrics := range before {
		if metrics.Readers != 2 || metrics.UserReaders != 2 || metrics.GuestReaders != 0 {
			t.Fatalf("unexpected fixture metrics: %+v", metrics)
		}
		metrics.UserReaders--
		metrics.GuestReaders++
		want[id] = metrics
	}
	for range 2 {
		if err := st.AnonymizeSubject(ctx, tenant, old, anonymous); err != nil {
			t.Fatal(err)
		}
		if after := metricsByID(t, st, tenant, []string{cid(1), cid(2)}, AllTime()); !reflect.DeepEqual(want, after) {
			t.Fatalf("anonymization changed totals beyond the user-to-anonymous split: want=%+v after=%+v", want, after)
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

type interruptedAnonymousDelete struct {
	Conn
	failed bool
}

func (c *interruptedAnonymousDelete) Exec(ctx context.Context, query string, args ...any) error {
	if !c.failed && strings.Contains(query, ".engagement_segments DELETE WHERE") {
		c.failed = true
		return errors.New("injected interruption after anonymous copy")
	}
	return c.Conn.Exec(ctx, query, args...)
}

func TestAnonymousCopyRetryPreservesDistinctRemovedReaders(t *testing.T) {
	st, conn := freshStore(t)
	ctx := t.Context()
	const tenant = "interrupted-anonymization"
	old := []Subject{{UserID: "alice"}, {UserID: "bob"}}
	for _, subject := range old {
		if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{view(tenant, cid(1), subject, 1, 10, 10, 10, 50, true)}); err != nil {
			t.Fatal(err)
		}
	}
	gate := &interruptedAnonymousDelete{Conn: conn}
	interrupted, _ := NewStore(gate, testDB)
	if err := interrupted.AnonymizeSubject(ctx, tenant, old[0], Subject{AnonKey: "fresh-alice"}); err == nil || !gate.failed {
		t.Fatalf("expected interruption after copying engagement: %v", err)
	}
	if n := countWhere(t, conn, "engagement_segments", "tenant=? AND subject_kind='anon' AND subject=?", tenant, "fresh-alice"); n != 1 {
		t.Fatalf("copy did not survive interrupted deletion: %d", n)
	}
	for i, subject := range old {
		anonymous := Subject{AnonKey: []string{"fresh-alice", "fresh-bob"}[i]}
		for range 2 {
			if err := st.AnonymizeSubject(ctx, tenant, subject, anonymous); err != nil {
				t.Fatal(err)
			}
		}
	}
	metrics := metricsByID(t, st, tenant, []string{cid(1)}, AllTime())[cid(1)]
	if metrics.Readers != 2 || metrics.UserReaders != 0 || metrics.GuestReaders != 2 {
		t.Fatalf("retry collapsed or duplicated anonymous contributions: %+v", metrics)
	}
}

func TestAnonymousExposureSurvivesMergeBeforeOldSubjectDeletion(t *testing.T) {
	st, conn := freshStore(t)
	ctx := t.Context()
	old, anonymous := Subject{UserID: "old-account"}, Subject{AnonKey: "fresh-anonymous-key"}
	if err := st.RecordExposures(ctx, "exposure", []Exposure{{RenderID: "render", Stage: StageServed, Subject: old,
		Shown: []Placement{{ContentRef: gallery("exposure", cid(1)), Position: 1}}, OccurredAt: at(1, 10)}}); err != nil {
		t.Fatal(err)
	}
	if err := st.copyAnonymous(ctx, "exposures", "exposure", old, anonymous, 1); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(ctx, "OPTIMIZE TABLE "+testDB+".exposures FINAL"); err != nil {
		t.Fatal(err)
	}
	if err := st.mutate(ctx, "exposures", "tenant=? AND subject_kind=? AND subject=?", "exposure", old.Kind(), old.Key()); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, conn, "exposures", "tenant=? AND subject_kind=? AND subject=?", "exposure", anonymous.Kind(), anonymous.Key()); n != 1 {
		t.Fatalf("merge discarded anonymous exposure: %d", n)
	}
}
