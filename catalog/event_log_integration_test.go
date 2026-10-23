package catalog

import (
	"encoding/json"
	"errors"
	"github.com/open-rails/helpers/eventlog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func emitFacts(t *testing.T, f *fixture, facts ...Event) {
	t.Helper()
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if err := f.s.emit(f.ctx, tx, facts...); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
}

func TestEventLogFiltersEmptyPagesAndCursorBoundaries(t *testing.T) {
	f := newFixture(t, nil)
	query := EventQuery{Types: []string{EventContentPublished}}
	empty, err := f.s.ListEvents(f.ctx, query)
	if err != nil || empty.NextCursor == "" || len(empty.Events) != 0 {
		t.Fatalf("empty stream: %+v %v", empty, err)
	}
	emitFacts(t, f, Event{Type: EventContentUpdated, Kind: "gallery", ContentID: "ignored"})
	query.After = empty.NextCursor
	filtered, err := f.s.ListEvents(f.ctx, query)
	if err != nil || len(filtered.Events) != 0 || filtered.HasMore || filtered.NextCursor == empty.NextCursor {
		t.Fatalf("filtered empty page did not advance: %+v %v", filtered, err)
	}
	emitFacts(t, f, Event{Type: EventContentPublished, Kind: "gallery", ContentID: "match"}, Event{Type: EventContentUpdated, Kind: "gallery", ContentID: "after"})
	query.After = filtered.NextCursor
	page, err := f.s.ListEvents(f.ctx, query)
	if err != nil || len(page.Events) != 1 || page.Events[0].ContentID != "match" {
		t.Fatalf("filtered continuation: %+v %v", page, err)
	}
	for name, q := range map[string]EventQuery{
		"changed filter": {After: page.NextCursor, Types: []string{EventContentUpdated}},
		"malformed":      {After: "not a cursor"},
	} {
		if _, err := f.s.ListEvents(f.ctx, q); !errors.Is(err, ErrEventCursorInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	cursor, _, err := decodeEventCursor(page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	foreign := cursor
	foreign.Scope = "another-app"
	if _, err := f.s.ListEvents(f.ctx, EventQuery{After: encodeEventCursor(foreign), Types: query.Types}); !errors.Is(err, ErrEventCursorInvalid) {
		t.Fatalf("cross-app cursor: %v", err)
	}
	for name, alter := range map[string]func(*eventCursor){
		"future":   func(c *eventCursor) { c.Position = "9000000" },
		"boundary": func(c *eventCursor) { c.Boundary = "different-history" },
		"epoch":    func(c *eventCursor) { c.Epoch = "another-restore" },
	} {
		invalid := cursor
		alter(&invalid)
		if _, err := f.s.ListEvents(f.ctx, EventQuery{After: encodeEventCursor(invalid), Types: query.Types}); !errors.Is(err, ErrEventCursorExpired) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestEventLogRetentionDoesNotSkipYoungerPrefix(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.EventRetention = time.Hour })
	emitFacts(t, f, Event{Type: EventContentCreated, Kind: "gallery", ContentID: "old-one"}, Event{Type: EventContentUpdated, Kind: "gallery", ContentID: "old-two"}, Event{Type: EventContentUpdated, Kind: "gallery", ContentID: "young"}, Event{Type: EventContentUpdated, Kind: "gallery", ContentID: "late-old-clock"})
	first, err := f.s.ListEvents(f.ctx, EventQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	f.advance(2 * time.Hour)
	// Simulate imported historical timestamps; runtime updates remain forbidden.
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err := tx.Exec(f.ctx, `ALTER TABLE `+f.table("eventlog_events")+` DISABLE TRIGGER eventlog_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE `+f.table("eventlog_events")+` SET recorded_at=$1 WHERE position=3`, f.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, `ALTER TABLE `+f.table("eventlog_events")+` ENABLE TRIGGER eventlog_immutable`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	all := readEventLog(t, f)
	if len(all) != 1 || all[0].ContentID != "young" {
		t.Fatalf("read-time retention: %+v", all)
	}
	if _, err := f.s.ListEvents(f.ctx, EventQuery{After: first.NextCursor}); !errors.Is(err, ErrEventCursorExpired) {
		t.Fatalf("expired before prune: %v", err)
	}
	if n, err := f.s.PruneEvents(f.ctx); err != nil || n != 2 {
		t.Fatalf("pruned %d: %v", n, err)
	}
	if _, err := f.s.ListEvents(f.ctx, EventQuery{After: first.NextCursor}); !errors.Is(err, ErrEventCursorExpired) {
		t.Fatalf("expired cursor silently continued: %v", err)
	}
	got := readEventLog(t, f)
	if len(got) != 1 || got[0].ContentID != "young" {
		t.Fatalf("retention skipped a younger prefix or later old clock: %+v", got)
	}
	beforeAllPrune, err := f.s.ListEvents(f.ctx, EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	f.advance(2 * time.Hour)
	if n, err := f.s.PruneEvents(f.ctx); err != nil || n != 2 {
		t.Fatalf("remaining prefix: %d %v", n, err)
	}
	if kept, err := f.s.ListEvents(f.ctx, EventQuery{After: beforeAllPrune.NextCursor}); err != nil || len(kept.Events) != 0 {
		t.Fatalf("a consumed boundary became invalid when pruned: %+v %v", kept, err)
	}
	page, err := f.s.ListEvents(f.ctx, EventQuery{})
	if err != nil || len(page.Events) != 0 || page.NextCursor == "" || page.RetentionSeconds != 3600 {
		t.Fatalf("empty retained stream: %+v %v", page, err)
	}
	if _, err := f.s.ListEvents(f.ctx, EventQuery{After: page.NextCursor}); err != nil {
		t.Fatalf("floor cursor unusable: %v", err)
	}
}

func TestEventLogStringIDsAndHTTPAuthorization(t *testing.T) {
	f := newFixture(t, nil)
	if _, err := f.pool.Exec(f.ctx, `SELECT setval($1::regclass,9007199254740992,true)`, f.table("content_event_ids")); err != nil {
		t.Fatal(err)
	}
	emitFacts(t, f, Event{Type: EventContentCreated, Kind: "gallery", ContentID: "large-id"})
	f.want(http.StatusUnauthorized, anon, "GET", "/v1/events", nil, nil)
	f.want(http.StatusForbidden, "bob", "GET", "/v1/events", nil, nil)
	var page EventPage
	f.want(http.StatusOK, "viewer", "GET", "/v1/events", nil, &page)
	if len(page.Events) != 1 || page.Events[0].ID != "9007199254740993" || page.Events[0].Version != 1 {
		t.Fatalf("large event ID lost precision: %+v", page)
	}
	body, err := json.Marshal(page)
	if err != nil || !strings.Contains(string(body), `"id":"9007199254740993"`) {
		t.Fatalf("numeric wire ID: %s %v", body, err)
	}
	f.want(http.StatusBadRequest, "viewer", "GET", "/v1/events?limit=501", nil, nil)
	f.want(http.StatusBadRequest, "viewer", "GET", "/v1/events?limit=0", nil, nil)
	cursor, position, err := decodeEventCursor(page.NextCursor)
	if err != nil || position != 1 {
		t.Fatalf("position is not separate from event ID: %s %v", page.NextCursor, err)
	}
	foreign := cursor
	foreign.Scope = "another-app"
	f.want(http.StatusBadRequest, "viewer", "GET", "/v1/events?after="+encodeEventCursor(foreign), nil, nil)
	cursor.Position = strconv.FormatInt(position+1, 10)
	f.want(http.StatusGone, "viewer", "GET", "/v1/events?after="+encodeEventCursor(cursor), nil, nil)
}

func TestEventLogRejectsReusedPositionAfterRestore(t *testing.T) {
	f := newFixture(t, nil)
	emitFacts(t, f, Event{Type: EventContentCreated, Kind: "gallery", ContentID: "stable"}, Event{Type: EventContentUpdated, Kind: "gallery", ContentID: "old-future"})
	before, err := f.s.ListEvents(f.ctx, EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	rollback, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback.Rollback(f.ctx)
	if _, err := rollback.Exec(f.ctx, `DELETE FROM `+f.table("eventlog_events")+` WHERE source='contentkit' AND scope=$1 AND position=2`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := rollback.Exec(f.ctx, `UPDATE `+f.table("eventlog_streams")+` SET position=1,head_id=(SELECT boundary FROM `+f.table("eventlog_events")+` WHERE source='contentkit' AND scope=$1 AND position=1) WHERE source='contentkit' AND scope=$1`, tenant); err != nil {
		t.Fatal(err)
	}
	if err := rollback.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	emitFacts(t, f, Event{Type: EventContentUpdated, Kind: "gallery", ContentID: "replacement"})
	if _, err := f.s.ListEvents(f.ctx, EventQuery{After: before.NextCursor}); !errors.Is(err, ErrEventCursorExpired) {
		t.Fatalf("accepted a cursor from an abandoned future: %v", err)
	}
	current, err := f.s.ListEvents(f.ctx, EventQuery{})
	if err != nil || len(current.Events) != 2 || current.Events[1].ContentID != "replacement" {
		t.Fatalf("new history unavailable: %+v %v", current, err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE `+f.table("eventlog_streams")+` SET epoch=gen_random_uuid()::text`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ListEvents(f.ctx, EventQuery{After: current.NextCursor}); !errors.Is(err, ErrEventCursorExpired) {
		t.Fatalf("accepted old stream epoch: %v", err)
	}
}

func TestSharedEventReaderPreservesLegacyCursorAndEnvelope(t *testing.T) {
	f := newFixture(t, nil)
	emitFacts(t, f, Event{Type: EventContentCreated, Kind: "gallery", ContentID: "one"}, Event{Type: EventContentUpdated, Kind: "gallery", ContentID: "two"})
	legacy, err := f.s.ListEvents(f.ctx, EventQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	cursor, _, err := decodeEventCursor(legacy.NextCursor)
	if err != nil || cursor.Boundary == legacy.Events[0].ID {
		t.Fatalf("legacy boundary lost: %+v %v", cursor, err)
	}
	shared, err := f.s.EventReader().ListEvents(f.ctx, eventlog.Query{After: legacy.NextCursor, Limit: 1})
	if err != nil || len(shared.Events) != 1 {
		t.Fatalf("shared continuation: %+v %v", shared, err)
	}
	e := shared.Events[0]
	if e.ID == legacy.Events[0].ID || e.Scope != tenant || e.Source != "contentkit" || e.Subject.ID != "two" || e.RecordedAt.IsZero() || e.OccurredAt.After(e.RecordedAt) {
		t.Fatalf("envelope: %+v", e)
	}
	if page, err := f.s.ListEvents(f.ctx, EventQuery{After: shared.NextCursor}); err != nil || len(page.Events) != 0 {
		t.Fatalf("legacy continuation: %+v %v", page, err)
	}
}

func TestSharedEventReaderFutureTypeFilterLimit(t *testing.T) {
	f := newFixture(t, nil)
	types := make([]string, eventlog.MaxTypes)
	for i := range types {
		types[i] = strings.Repeat("x", 120) + strconv.Itoa(i)
	}
	page, err := f.s.EventReader().ListEvents(f.ctx, eventlog.Query{Types: types})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("future types: %+v %v", page, err)
	}
	if _, err := f.s.EventReader().ListEvents(f.ctx, eventlog.Query{Types: types, After: page.NextCursor}); err != nil {
		t.Fatalf("large cursor round trip: %v", err)
	}
	if _, err := f.s.EventReader().ListEvents(f.ctx, eventlog.Query{Types: append(types, "overflow")}); !errors.Is(err, eventlog.ErrInvalidQuery) {
		t.Fatalf("type limit: %v", err)
	}
}
