package critical_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
)

func TestErasureRecoveryMode(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	t.Run("CK-J23-S05_recovery_engine_refuses_serving_jobs_and_host_client", func(t *testing.T) {
		recovery, err := ck.NewForErasureReplay(t.Context(), a.cfg, a.deps)
		must(t, err)
		defer func() { must(t, recovery.Close(context.Background())) }()
		if _, err = recovery.Routes(ck.Routes{Groups: recovery.RouteGroups()}); !errors.Is(err, ck.ErrErasureReplayRequired) {
			t.Fatalf("recovery routes: %v", err)
		}
		if err = recovery.Start(t.Context()); !errors.Is(err, ck.ErrErasureReplayRequired) {
			t.Fatalf("recovery Start: %v", err)
		}
		if _, err = recovery.Client().GetKinds(t.Context()); !errors.Is(err, ck.ErrErasureReplayRequired) {
			t.Fatalf("recovery host client: %v", err)
		}
		report, err := recovery.ReplayErasureFences(t.Context())
		must(t, err)
		if !report.Complete() {
			t.Fatalf("empty replay incomplete: %+v", report)
		}
		// Recovery is a permanent mode of this instance, even after replay.
		if _, err = recovery.Client().GetKinds(t.Context()); !errors.Is(err, ck.ErrErasureReplayRequired) {
			t.Fatalf("replayed instance became a serving host: %v", err)
		}
		_, err = a.client("").GetKinds(t.Context())
		must(t, err)
	})
}

func TestClickHouseErasureRetry(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	addr, user, password := os.Getenv("CONTENTKIT_CRITICAL_CH_ADDR"), os.Getenv("CONTENTKIT_CRITICAL_CH_USER"), os.Getenv("CONTENTKIT_CRITICAL_CH_PASSWORD")
	if addr == "" || user == "" || password == "" {
		t.Fatal("real owned ClickHouse settings are required for selected erasure tests")
	}
	database := w.schema + "_signals"
	a.cfg.ClickHouse = ck.ClickHouseConfig{Addr: addr, Username: user, Password: password, Database: database, Cluster: os.Getenv("CONTENTKIT_CRITICAL_CH_CLUSTER")}
	must(t, ck.MigrateClickHouse(t.Context(), w.pool, a.cfg))
	direct, err := clickhouse.Open(&clickhouse.Options{Addr: []string{addr}, Auth: clickhouse.Auth{Database: database, Username: user, Password: password}, DialTimeout: time.Second})
	must(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		a.server.Close()
		if err := a.engine.Close(ctx); err != nil {
			t.Errorf("close owned signal runtime: %v", err)
		}
		if err := direct.Exec(ctx, "DROP DATABASE IF EXISTS "+database+" SYNC"); err != nil {
			t.Errorf("drop owned ClickHouse fixture: %v", err)
		}
		must(t, direct.Close())
	})
	wire := newTCPCut(t, addr)
	borrowed, err := clickhouse.Open(&clickhouse.Options{Addr: []string{wire.listener.Addr().String()}, Auth: clickhouse.Auth{Database: database, Username: user, Password: password}, DialTimeout: time.Second, ReadTimeout: 2 * time.Second})
	must(t, err)
	t.Cleanup(func() { must(t, borrowed.Close()) })
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.cfg.Database.SkipMigrations = true
	a.deps.ClickHouse = borrowed
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	alice, aliceToken := w.person(t, 0)
	bob, bobToken := w.person(t, 0)
	item := publishedArticle(t, a, "durable signal erasure")
	partition := a.cfg.Scope.AppID.String()
	for _, token := range []string{aliceToken, bobToken} {
		opened(t, a.client(token), item.ID)
		_, err = a.client(token).PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
		must(t, err)
	}
	present := func(id string) bool { return logged(t, direct, database, partition, uuid.MustParse(id)) > 0 }
	waitForWithin(t, 30*time.Second, func() bool { return present(alice.ID) && present(bob.ID) })
	t.Run("CK-J23-S03_signal_outage_cannot_report_completed_erasure", func(t *testing.T) {
		wire.cut()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := a.engine.AnonymizeAccount(ctx, alice.ID); err == nil {
			t.Fatal("unavailable CH falsely completed account deletion")
		}
		_, err = a.client(aliceToken).PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
		requireError(t, err, 403, "forbidden")
		current, err := a.client(bobToken).GetContentFavorite(t.Context(), item.ID, ck.GetContentFavoriteQuery{})
		must(t, err)
		if !current.Favorited {
			t.Fatal("failed erasure spilled into sibling account")
		}
		// The activity really existed at the outage; a fake empty-plane
		// success would not establish a retry obligation.
		if !present(alice.ID) {
			t.Fatal("alice's activity vanished without a completed erasure")
		}
	})
	must(t, a.engine.Close(t.Context()))
	a.deps.ClickHouse = nil
	recovery, err := ck.NewForErasureReplay(t.Context(), a.cfg, a.deps)
	must(t, err)
	t.Run("CK-J23-S06_replay_clears_only_target_app_subject", func(t *testing.T) {
		report, err := recovery.ReplayErasureFences(t.Context())
		must(t, err)
		if !report.Complete() {
			t.Fatalf("healthy retry incomplete: %+v", report)
		}
		if present(alice.ID) || !present(bob.ID) {
			t.Fatalf("after replay alice readable=%v, bob readable=%v", present(alice.ID), present(bob.ID))
		}
		report, err = recovery.ReplayErasureFences(t.Context())
		must(t, err)
		if !report.Complete() {
			t.Fatal("repeated replay regressed")
		}
	})
	must(t, recovery.Close(t.Context()))
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server.Close()
	a.server = mountEngine(t, a.engine)
	x := &analyticsWorld{world: w, a: a}
	t.Run("CK-J23-S07_late_activity_cannot_rebuild_an_erased_subject", func(t *testing.T) {
		// Ingestion may accept an erased subject's late row; reads drop it and
		// the job never folds it. The live sibling is the control.
		opened(t, a.client(aliceToken), item.ID)
		opened(t, a.client(bobToken), item.ID)
		waitForWithin(t, 30*time.Second, func() bool { return logged(t, direct, database, partition, uuid.MustParse(bob.ID)) >= 2 })
		if present(alice.ID) {
			t.Fatal("an erased subject's late row is readable")
		}
		startPublicationRuntime(t, a)
		x.runJob(t)
		if _, ok := x.state(t, uuid.MustParse(alice.ID), item.ID); ok {
			t.Fatal("the job folded an erased subject's late row")
		}
		if _, ok := x.state(t, uuid.MustParse(bob.ID), item.ID); !ok {
			t.Fatal("the live control has no reading state")
		}
	})
}
