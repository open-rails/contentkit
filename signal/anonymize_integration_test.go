package signal

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/signaltest"
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

type anonymousReplicaSwitch struct {
	Conn
	copySource Conn
	copied     chan struct{}
	once       sync.Once
}

func (c *anonymousReplicaSwitch) Exec(ctx context.Context, query string, args ...any) error {
	if strings.HasPrefix(query, "INSERT INTO "+testDB+".engagement_segments (") && strings.Contains(query, " FINAL") {
		c.once.Do(func() { close(c.copied) })
		return c.copySource.Exec(ctx, query, args...)
	}
	return c.Conn.Exec(ctx, query, args...)
}

func TestAnonymousCopyWaitsForEveryReplicaBeforeSwitchingSource(t *testing.T) {
	env := signaltest.FromEnv(t)
	envB := env
	envB.Addr = os.Getenv("CONTENTKIT_TEST_CH_ADDR2")
	if envB.Addr == "" || env.Cluster == "" {
		t.Skip("two replicas and CONTENTKIT_TEST_CH_CLUSTER are required")
	}
	stA, connA := freshStore(t)
	connB := envB.Open(t, testDB)
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
	defer cancel()
	quorum, err := stA.erasureQuorum(ctx)
	if err != nil || quorum != 2 {
		t.Fatalf("expected two replicas: quorum=%d err=%v", quorum, err)
	}
	if err := connB.Exec(ctx, "SYSTEM STOP FETCHES "+testDB+".engagement_segments"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connB.Exec(context.Background(), "SYSTEM START FETCHES "+testDB+".engagement_segments") })
	old := Subject{UserID: "removed-reader"}
	const tenant = "replica-switch"
	if err := recordFixtureSignals(stA, ctx, tenant, []readingFixture{view(tenant, cid(1), old, 1, 10, 10, 10, 50, true)}); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, connB, "engagement_segments", "tenant=?", tenant); n != 0 {
		t.Fatalf("replica B must be behind before copying: %d", n)
	}
	routed := &anonymousReplicaSwitch{Conn: connA, copySource: connB, copied: make(chan struct{})}
	st, err := NewStore(routed, testDB, StoreOptions{Cluster: env.Cluster})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- st.AnonymizeSubject(ctx, tenant, old, Subject{AnonKey: "fresh-anonymous-reader"}) }()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		rows, err := connB.Query(ctx, `SELECT count() FROM system.processes WHERE query_kind='System' AND position(query,'SYSTEM SYNC REPLICA')>0 AND position(query,?)>0`, testDB+".engagement_segments")
		if err != nil {
			t.Fatal(err)
		}
		var syncing uint64
		if rows.Next() {
			err = rows.Scan(&syncing)
		}
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		if syncing > 0 {
			break
		}
		select {
		case <-routed.copied:
			t.Fatal("copy reached stale B before cluster synchronization")
		case err := <-done:
			t.Fatalf("anonymization ended before blocked B synchronized: %v", err)
		case <-deadline.C:
			t.Fatal("cluster synchronization never reached replica B")
		case <-time.After(25 * time.Millisecond):
		}
	}
	select {
	case <-routed.copied:
		t.Fatal("copy did not wait for B's stopped fetches")
	case <-time.After(100 * time.Millisecond):
	}
	if err := connB.Exec(ctx, "SYSTEM START FETCHES "+testDB+".engagement_segments"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-routed.copied:
	default:
		t.Fatal("copy never used replica B")
	}
	for _, conn := range []Conn{connA, connB} {
		store, _ := NewStore(conn, testDB)
		metrics := metricsByID(t, store, tenant, []string{cid(1)}, AllTime())[cid(1)]
		if metrics.Readers != 1 || metrics.GuestReaders != 1 || metrics.UserReaders != 0 {
			t.Fatalf("replica lost anonymized readership: %+v", metrics)
		}
		if n := countWhere(t, conn, "engagement_segments", "tenant=? AND subject_kind='user'", tenant); n != 0 {
			t.Fatalf("old reader remains after completion: %d", n)
		}
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
