package signal

import (
	"context"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/signaltest"
)

func TestIntegrationViewRecencyMigrationBackfillsExistingState(t *testing.T) {
	env := signaltest.FromEnv(t)
	ctx := context.Background()
	conn := env.Empty(t, testDB)
	env.ApplyRange(t, conn, 0, 5)

	// This is the compacted state an installation has before 0006: a later
	// click moved last_signal_at forward, while the latest consumption was
	// still the view on May 4.
	if err := conn.Exec(ctx, `
        INSERT INTO subject_content_state
          (tenant, subject_kind, subject, content_kind, content_id,
           first_seen_at, last_signal_at, total_events, views, completions,
           active_s, max_progress, progress_max, completed, resume, last_score,
           net_value, feedback, version)
        VALUES
          ('doujins', 'user', 'u1', 'gallery', 'g1',
           toDateTime('2026-05-01 10:00:00'), toDateTime('2026-05-05 10:00:00'),
           3, 2, 0, 30, 4, 20, false, 'p:4', 0, 0, 0,
           toDateTime64('2026-05-05 10:00:01', 6))`); err != nil {
		t.Fatal(err)
	}

	if err := conn.Exec(ctx, `
        INSERT INTO signals
          (tenant, content_kind, content_id, subject_kind, subject,
           signal_type, event_id, occurred_at, progress, progress_max)
        VALUES
          ('doujins', 'gallery', 'g1', 'user', 'u1', 'view', 'v1',
           toDateTime('2026-05-02 10:00:00'), 2, 20),
          ('doujins', 'gallery', 'g1', 'user', 'u1', 'view', 'v2',
           toDateTime('2026-05-04 10:00:00'), 4, 20),
          ('doujins', 'gallery', 'g1', 'user', 'u1', 'click', 'c1',
           toDateTime('2026-05-05 10:00:00'), 0, 0)
    `); err != nil {
		t.Fatal(err)
	}

	env.ApplyRange(t, conn, 5, 6)

	var got time.Time
	if err := conn.QueryRow(ctx, `SELECT last_view_at FROM subject_content_state FINAL WHERE tenant='doujins' AND subject_kind='user' AND subject='u1' AND content_kind='gallery' AND content_id='g1'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("backfilled last_view_at = %v, want %v", got, want)
	}

	st, err := NewStore(conn, testDB)
	if err != nil {
		t.Fatal(err)
	}
	history, err := st.History(ctx, "doujins", Subject{UserID: "u1"}, HistoryOptions{
		Status: HistorySeen,
		Since:  time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].ContentID != "g1" {
		t.Fatalf("backfilled watched history = %+v, want g1", history)
	}
}
