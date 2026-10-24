package media_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/helpers/eventlog"
	eventpg "github.com/open-rails/helpers/eventlog/postgres"

	"github.com/open-rails/contentkit/media"
)

// A processor may die after its durable observation commits and before River
// finishes its job. Retrying must preserve exactly one event and metadata effect.
func TestProcessingCompletionAtomicAndRetryable(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.gallery(1, 1)
	ctx := t.Context()
	schema := f.env.ContentSchema()
	pool := f.env.Pool()
	effects := pgx.Identifier{schema, "processing_test_effects"}.Sanitize()
	if _, err := pool.Exec(ctx, `CREATE TABLE `+effects+`(id int PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("interrupt completion")
	complete := func(fail bool) error {
		return media.CompleteProcessing(ctx, f.ms, ref, func(ctx context.Context, tx pgx.Tx, r media.Readiness) error {
			if !r.Ready() {
				t.Fatalf("readiness %+v", r)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO `+effects+` VALUES(1) ON CONFLICT DO NOTHING`); err != nil {
				return err
			}
			if fail {
				return failed
			}
			return nil
		})
	}
	reader, err := eventpg.New(pool, eventpg.Config{Schema: schema, Source: "contentkit", Scope: ref.TenantID})
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.EnsureScope(ctx); err != nil {
		t.Fatal(err)
	}
	events := func() eventlog.Page {
		t.Helper()
		page, err := reader.ListEvents(ctx, eventlog.Query{Types: []string{media.EventProcessingCompleted}})
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	if err := complete(true); !errors.Is(err, failed) {
		t.Fatalf("interrupted completion: %v", err)
	}
	if got := events(); len(got.Events) != 0 {
		t.Fatalf("rollback kept events: %+v", got)
	}
	pending := pgx.Identifier{schema, "eventlog_pending"}.Sanitize()
	if _, err := pool.Exec(ctx, `ALTER TABLE `+pending+` ADD CONSTRAINT processing_test_failure CHECK (event_type<>'media.processing_completed')`); err != nil {
		t.Fatal(err)
	}
	if err := complete(false); err == nil {
		t.Fatal("event append fault was ignored")
	}
	var rolledBack int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+effects).Scan(&rolledBack); err != nil || rolledBack != 0 {
		t.Fatalf("event failure committed metadata: %d %v", rolledBack, err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE `+pending+` DROP CONSTRAINT processing_test_failure`); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := complete(false); err != nil {
			t.Fatal(err)
		}
	}
	got := events()
	if len(got.Events) != 1 {
		t.Fatalf("retry events=%+v", got)
	}
	body := string(got.Events[0].Data)
	var fact media.ProcessingEvent
	if err := json.Unmarshal(got.Events[0].Data, &fact); err != nil {
		t.Fatal(err)
	}
	if fact.ContentID != ref.ContentID || fact.State != media.StateReady || strings.Contains(body, "https:") || strings.Contains(body, "private/") {
		t.Fatalf("completion data: %s", body)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+effects).Scan(&count); err != nil || count != 1 {
		t.Fatalf("metadata effects=%d: %v", count, err)
	}
	// Repeating the same observation still repairs real metadata independently
	// of event deduplication (for example after its earlier job failed).
	if _, err := pool.Exec(ctx, `DELETE FROM `+effects); err != nil {
		t.Fatal(err)
	}
	if err := complete(false); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+effects).Scan(&count); err != nil || count != 1 {
		t.Fatalf("unchanged observation skipped metadata repair: %d %v", count, err)
	}
	if got := events(); len(got.Events) != 1 {
		t.Fatalf("metadata repair duplicated event: %+v", got)
	}
	// Exercise the shared retention path; the current observation receipt remains.
	expired, err := eventpg.New(pool, eventpg.Config{Schema: schema, Source: "contentkit", Scope: ref.TenantID, Now: func() time.Time { return time.Now().Add(eventlog.DefaultRetention + time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := expired.Prune(ctx, 1000); err != nil || n != 1 {
		t.Fatalf("expired events=%d: %v", n, err)
	}
	if err := complete(false); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{schema, "eventlog_events"}.Sanitize()+` WHERE source='contentkit' AND scope=$1`, ref.TenantID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("old completion reappeared after retention=%d: %v", count, err)
	}
}

func TestProcessingEventsReportChangedCountsAndSources(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ref := f.ref("gallery", 1)
	ctx := t.Context()
	f.put(ref, "originals/1.png", "image/png", png(1))
	complete := func() {
		t.Helper()
		if err := media.CompleteProcessing(ctx, f.ms, ref, nil); err != nil {
			t.Fatal(err)
		}
	}
	complete()
	complete()
	f.produce(ref)
	complete()
	complete()
	f.put(ref, "originals/1.png", "image/png", png(2))
	f.produce(ref)
	complete()
	reader, err := eventpg.New(f.env.Pool(), eventpg.Config{Schema: f.env.ContentSchema(), Source: "contentkit", Scope: ref.TenantID})
	if err != nil {
		t.Fatal(err)
	}
	page, err := reader.ListEvents(ctx, eventlog.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 3 || page.Events[0].Type != media.EventProcessingProgressed || page.Events[1].Type != media.EventProcessingCompleted || page.Events[2].Type != media.EventProcessingCompleted {
		t.Fatalf("processing transitions %+v", page)
	}
}
