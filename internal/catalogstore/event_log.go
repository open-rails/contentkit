package catalogstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	catalog "github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/helpers/eventlog"
	eventpg "github.com/open-rails/helpers/eventlog/postgres"
)

var (
	ErrEventCursorInvalid = errors.New("catalog: invalid event cursor")
)

var (
	ErrEventCursorExpired = errors.New("catalog: event cursor expired; resync current state")
)

type eventCursor struct {
	Version  int      `json:"v"`
	Scope    string   `json:"s"`
	Epoch    string   `json:"e"`
	Position string   `json:"p"`
	Boundary string   `json:"b,omitempty"`
	Types    []string `json:"t,omitempty"`
}

func decodeEventCursor(value string) (eventCursor, int64, error) {
	var c eventCursor
	if len(value) > 32768 {
		return c, 0, ErrEventCursorInvalid
	}
	body, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return c, 0, ErrEventCursorInvalid
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF || c.Version != 1 || c.Scope == "" || c.Epoch == "" {
		return c, 0, ErrEventCursorInvalid
	}
	position, err := strconv.ParseInt(c.Position, 10, 64)
	if err != nil || position < 0 || strconv.FormatInt(position, 10) != c.Position {
		return c, 0, ErrEventCursorInvalid
	}
	return c, position, nil
}

func encodeEventCursor(c eventCursor) string {
	body, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(body)
}

// ListEvents preserves the catalog wire format over the shared paging engine.
func (s *Store) ListEvents(ctx context.Context, query catalog.EventQuery) (catalog.EventPage, error) {
	page, err := s.EventReader().ListEvents(ctx, eventlog.Query{After: query.After, Limit: query.Limit, Types: query.Types})
	result := catalog.EventPage{Events: []catalog.EventRecord{}, NextCursor: page.NextCursor, HasMore: page.HasMore, RetentionSeconds: page.RetentionSeconds}
	if err != nil {
		switch {
		case errors.Is(err, eventlog.ErrInvalidQuery), errors.Is(err, eventlog.ErrInvalidCursor):
			return result, fmt.Errorf("%w: %w", ErrEventCursorInvalid, err)
		case errors.Is(err, eventlog.ErrExpiredCursor):
			return result, fmt.Errorf("%w: %w", ErrEventCursorExpired, err)
		default:
			return result, err
		}
	}
	for _, e := range page.Events {
		var fact eventPayload
		if err := json.Unmarshal(e.Data, &fact); err != nil {
			return result, err
		}
		result.Events = append(result.Events, catalog.EventRecord{ID: e.ID, Version: e.Version, Type: e.Type, Kind: fact.Kind, ContentID: fact.ContentID, VersionID: fact.VersionID,
			EntityID: fact.EntityID, MergedInto: fact.MergedInto, OccurredAt: e.OccurredAt, RecordedAt: e.RecordedAt, Source: e.Source, Scope: e.Scope, Data: e.Data})
	}
	return result, nil
}

// EventReader is bound to this store's app. Public callers use Client.EventReader,
// which retains the client's authorization and transport behavior.
func (s *Store) EventReader() eventlog.Reader {
	return eventlog.Log{Source: "contentkit", Scope: s.tenant, Retention: s.eventRetention, Now: s.now, Codec: catalogCursorCodec{}, ReadSnapshot: func(ctx context.Context, read func(eventlog.Snapshot) error) error {
		tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if err := read(&catalogSnapshot{store: s, tx: tx}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}}
}

type catalogCursorCodec struct{}

func (catalogCursorCodec) Encode(c eventlog.Cursor) string {
	return encodeEventCursor(eventCursor{Version: c.Version, Scope: c.Scope, Epoch: c.Epoch, Position: strconv.FormatInt(c.Position, 10), Boundary: c.Boundary, Types: c.Types})
}

func (catalogCursorCodec) Decode(raw string) (eventlog.Cursor, error) {
	c, p, err := decodeEventCursor(raw)
	if err != nil {
		return eventlog.Cursor{}, eventlog.ErrInvalidCursor
	}
	return eventlog.Cursor{Version: c.Version, Source: "contentkit", Scope: c.Scope, Epoch: c.Epoch, Position: p, Boundary: c.Boundary, Types: c.Types}, nil
}

type catalogSnapshot struct {
	store  *Store
	tx     pgx.Tx
	cutoff time.Time
	head   int64
}

func (s *catalogSnapshot) Stream(ctx context.Context, cutoff time.Time) (eventlog.Stream, error) {
	s.cutoff = cutoff
	var state eventlog.Stream
	if err := s.tx.QueryRow(ctx, `SELECT epoch::text FROM `+s.store.qs+`.content_event_epoch WHERE singleton`).Scan(&state.Epoch); err != nil {
		return state, err
	}
	// Public catalog cursors have always used the schema-global epoch. A
	// persisted stream can predate a global reset, including first creation by
	// a repeatable-read transaction with an older snapshot.
	err := s.tx.QueryRow(ctx, `SELECT position,pruned_through,pruned_id FROM `+s.store.qs+`.eventlog_streams WHERE source='contentkit' AND scope=$1`, s.store.tenant).Scan(&state.Head.Number, &state.Floor.Number, &state.Floor.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return state, err
	}
	s.head = state.Head.Number
	// Advance only the expired prefix, and exclude later expired rows separately.
	var floor int64
	var boundary string
	err = s.tx.QueryRow(ctx, `SELECT position,boundary FROM `+s.store.qs+`.eventlog_events WHERE source='contentkit' AND scope=$1 AND position>$2 AND position<coalesce((SELECT min(position) FROM `+s.store.qs+`.eventlog_events WHERE source='contentkit' AND scope=$1 AND position>$2 AND recorded_at >= $3),$4+1) ORDER BY position DESC LIMIT 1`, s.store.tenant, state.Floor.Number, cutoff, s.head).Scan(&floor, &boundary)
	if err == nil {
		state.Floor = eventlog.Position{Number: floor, ID: boundary}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return state, err
	}
	state.Head.ID = state.Floor.ID
	if state.Head.Number > state.Floor.Number {
		state.Head.ID, err = s.Boundary(ctx, state.Head.Number)
		if err != nil {
			return state, err
		}
	}
	return state, nil
}

func (s *catalogSnapshot) Boundary(ctx context.Context, position int64) (string, error) {
	var id string
	err := s.tx.QueryRow(ctx, `SELECT boundary FROM `+s.store.qs+`.eventlog_events WHERE source='contentkit' AND scope=$1 AND position=$2`, s.store.tenant, position).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", eventlog.ErrExpiredCursor
	}
	return id, err
}

func (s *catalogSnapshot) Events(ctx context.Context, after int64, types []string, limit int) ([]eventlog.Record, error) {
	rows, err := s.tx.Query(ctx, `SELECT id,version,event_type,data,occurred_at,recorded_at,position,boundary FROM `+s.store.qs+`.eventlog_events WHERE source='contentkit' AND scope=$1 AND position>$2 AND position<=$3 AND recorded_at >= $4 AND (cardinality($5::text[])=0 OR event_type=ANY($5)) ORDER BY position LIMIT $6`, s.store.tenant, after, s.head, s.cutoff, append([]string{}, types...), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []eventlog.Record{}
	for rows.Next() {
		var r eventlog.Record
		if err := rows.Scan(&r.Event.ID, &r.Event.Version, &r.Event.Type, &r.Event.Data, &r.Event.OccurredAt, &r.Event.RecordedAt, &r.Position, &r.Boundary); err != nil {
			return nil, err
		}
		var fact eventPayload
		if err := json.Unmarshal(r.Event.Data, &fact); err != nil {
			return nil, err
		}
		r.Event.Source, r.Event.Scope = "contentkit", s.store.tenant

		r.Event.Subject = eventlog.Subject{Type: fact.Kind, ID: fact.subject()}
		result = append(result, r)
	}
	return result, rows.Err()
}

// PruneEvents bounds maintenance while reads independently enforce retention.
func (s *Store) PruneEvents(ctx context.Context) (int, error) {
	store, err := eventpg.New(s.pool, eventpg.Config{Schema: s.schema, Source: "contentkit", Scope: s.tenant, Retention: s.eventRetention, Now: s.now})
	if err != nil {
		return 0, err
	}
	count, err := store.Prune(ctx, 1000)
	return int(count), err
}
