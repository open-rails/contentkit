package catalog

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
	"github.com/open-rails/helpers/eventlog"
)

// DefaultEventRetention is independent of readers' progress.
const DefaultEventRetention = 90 * 24 * time.Hour

var (
	ErrEventCursorInvalid = errors.New("catalog: invalid event cursor")
	ErrEventCursorExpired = errors.New("catalog: event cursor expired; resync current state")
)

// EventQuery reads one app's retained log. Callers own their continuation.
type EventQuery struct {
	After string
	Limit int
	Types []string
}

// EventRecord is an immutable committed catalog fact. IDs are decimal strings
// so JSON clients do not lose bigint precision.
type EventRecord struct {
	ID         string          `json:"id"`
	Version    int             `json:"version"`
	Type       string          `json:"type"`
	Kind       string          `json:"kind"`
	ContentID  string          `json:"content_id"`
	VersionID  string          `json:"version_id,omitempty"`
	OccurredAt time.Time       `json:"occurred_at"`
	RecordedAt time.Time       `json:"recorded_at"`
	Source     string          `json:"source"`
	Scope      string          `json:"scope"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// EventPage always provides a continuation, including an empty stream.
type EventPage struct {
	Events           []EventRecord `json:"events"`
	NextCursor       string        `json:"next_cursor"`
	HasMore          bool          `json:"has_more"`
	RetentionSeconds int64         `json:"retention_seconds"`
}

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
func (s *Store) ListEvents(ctx context.Context, query EventQuery) (EventPage, error) {
	page, err := s.EventReader().ListEvents(ctx, eventlog.Query{After: query.After, Limit: query.Limit, Types: query.Types})
	result := EventPage{Events: []EventRecord{}, NextCursor: page.NextCursor, HasMore: page.HasMore, RetentionSeconds: page.RetentionSeconds}
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
		result.Events = append(result.Events, EventRecord{ID: e.ID, Version: e.Version, Type: e.Type, Kind: fact.Kind, ContentID: fact.ContentID, VersionID: fact.VersionID, OccurredAt: e.OccurredAt, RecordedAt: e.RecordedAt, Source: e.Source, Scope: e.Scope, Data: e.Data})
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
	err := s.tx.QueryRow(ctx, `SELECT position,retained_after,coalesce(retained_boundary::text,'') FROM `+s.store.qs+`.content_event_streams WHERE tenant_id=$1`, s.store.tenant).Scan(&state.Head.Number, &state.Floor.Number, &state.Floor.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return state, err
	}
	s.head = state.Head.Number
	// Advance only the expired prefix, and exclude later expired rows separately.
	var floor int64
	var boundary string
	err = s.tx.QueryRow(ctx, `SELECT position,boundary::text FROM `+s.store.t.events+` WHERE tenant_id=$1 AND position>$2 AND position<coalesce((SELECT min(position) FROM `+s.store.t.events+` WHERE tenant_id=$1 AND position>$2 AND created_at >= $3),$4+1) ORDER BY position DESC LIMIT 1`, s.store.tenant, state.Floor.Number, cutoff, s.head).Scan(&floor, &boundary)
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
	err := s.tx.QueryRow(ctx, `SELECT boundary::text FROM `+s.store.t.events+` WHERE tenant_id=$1 AND position=$2`, s.store.tenant, position).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", eventlog.ErrExpiredCursor
	}
	return id, err
}
func (s *catalogSnapshot) Events(ctx context.Context, after int64, types []string, limit int) ([]eventlog.Record, error) {
	rows, err := s.tx.Query(ctx, `SELECT id::text,event_version,type,payload,created_at,position,boundary::text FROM `+s.store.t.events+` WHERE tenant_id=$1 AND position>$2 AND position<=$3 AND created_at >= $4 AND (cardinality($5::text[])=0 OR type=ANY($5)) ORDER BY position LIMIT $6`, s.store.tenant, after, s.head, s.cutoff, append([]string{}, types...), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []eventlog.Record{}
	for rows.Next() {
		var r eventlog.Record
		if err := rows.Scan(&r.Event.ID, &r.Event.Version, &r.Event.Type, &r.Event.Data, &r.Event.RecordedAt, &r.Position, &r.Boundary); err != nil {
			return nil, err
		}
		var fact eventPayload
		if err := json.Unmarshal(r.Event.Data, &fact); err != nil {
			return nil, err
		}
		r.Event.Source, r.Event.Scope = "contentkit", s.store.tenant
		// The legacy log recorded mutation facts at insertion time; both timestamps
		// coincide. Keep the fields distinct for consumers and future storage.
		r.Event.OccurredAt = r.Event.RecordedAt
		r.Event.Subject = eventlog.Subject{Type: fact.Kind, ID: fact.ContentID}
		result = append(result, r)
	}
	return result, rows.Err()
}

// PruneEvents bounds each maintenance run and advances only a contiguous
// expired prefix. A younger event never gets skipped by the retained floor.
func (s *Store) PruneEvents(ctx context.Context) (int, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var floor int64
	if err := tx.QueryRow(ctx, `SELECT retained_after FROM `+s.qs+`.content_event_streams WHERE tenant_id=$1 FOR UPDATE`, s.tenant).Scan(&floor); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := s.now().Add(-s.eventRetention)
	rows, err := tx.Query(ctx, `SELECT position,boundary::text,created_at FROM `+s.t.events+` WHERE tenant_id=$1 AND position>$2 ORDER BY position LIMIT 1000`, s.tenant, floor)
	if err != nil {
		return 0, err
	}
	var through int64
	var boundary string
	count := 0
	for rows.Next() {
		var position int64
		var token string
		var at time.Time
		if err := rows.Scan(&position, &token, &at); err != nil {
			rows.Close()
			return 0, err
		}
		if !at.Before(cutoff) {
			break
		}
		through, boundary, count = position, token, count+1
	}
	rows.Close()
	if err := rows.Err(); err != nil || count == 0 {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM `+s.t.events+` WHERE tenant_id=$1 AND position<=$2`, s.tenant, through); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE `+s.qs+`.content_event_streams SET retained_after=$2,retained_boundary=$3 WHERE tenant_id=$1`, s.tenant, through, boundary); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}
