package media_test

import (
	"context"
	"errors"
	"strings"
	"testing"

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
			if _, err := tx.Exec(ctx, `INSERT INTO `+effects+` VALUES(1)`); err != nil {
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
	if !strings.Contains(body, `"content_id":"`+ref.ContentID+`"`) || !strings.Contains(body, `"state":"ready"`) || strings.Contains(body, "https:") || strings.Contains(body, "private/") {
		t.Fatalf("completion data: %s", body)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+effects).Scan(&count); err != nil || count != 1 {
		t.Fatalf("metadata effects=%d: %v", count, err)
	}
	// Simulate retained facts expiring: the current observation receipt remains.
	if _, err := pool.Exec(ctx, `DELETE FROM `+pgx.Identifier{schema, "eventlog_events"}.Sanitize()+` WHERE source='contentkit' AND scope=$1`, ref.TenantID); err != nil {
		t.Fatal(err)
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
