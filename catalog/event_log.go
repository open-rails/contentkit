package catalog

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
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
	ID         string    `json:"id"`
	Version    int       `json:"version"`
	Type       string    `json:"type"`
	Kind       string    `json:"kind"`
	ContentID  string    `json:"content_id"`
	VersionID  string    `json:"version_id,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
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

func eventTypes(in []string) ([]string, error) {
	if len(in) > 32 {
		return nil, fmt.Errorf("%w: too many event types", ErrEventCursorInvalid)
	}
	valid := map[string]bool{
		EventContentCreated: true, EventContentUpdated: true, EventContentScheduled: true,
		EventContentPublished: true, EventContentUnpublished: true, EventContentDeleted: true,
		EventContentRestored: true, EventContentPurged: true, EventVersionCreated: true,
		EventVersionUpdated: true, EventVersionPublished: true, EventVersionDeleted: true, EventVersionPurged: true,
	}
	out := append([]string{}, in...)
	for _, kind := range out {
		if !valid[kind] {
			return nil, fmt.Errorf("%w: unknown event type %q", ErrEventCursorInvalid, kind)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func decodeEventCursor(value string) (eventCursor, int64, error) {
	var c eventCursor
	if len(value) > 4096 {
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

// ListEvents does not mark rows delivered, register a reader, or start a worker.
// The repeatable-read snapshot binds pagination to one committed stream head.
func (s *Store) ListEvents(ctx context.Context, query EventQuery) (EventPage, error) {
	page := EventPage{Events: []EventRecord{}, RetentionSeconds: int64(s.eventRetention / time.Second)}
	limit := query.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 {
		return page, fmt.Errorf("%w: limit must be between 1 and 500", ErrEventCursorInvalid)
	}
	types, err := eventTypes(query.Types)
	if err != nil {
		return page, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return page, err
	}
	defer tx.Rollback(ctx)
	var epoch string
	if err := tx.QueryRow(ctx, `SELECT epoch::text FROM `+s.qs+`.content_event_epoch WHERE singleton`).Scan(&epoch); err != nil {
		return page, err
	}
	var head, floor int64
	var floorBoundary string
	err = tx.QueryRow(ctx, `SELECT position,retained_after,coalesce(retained_boundary::text,'') FROM `+s.qs+`.content_event_streams WHERE tenant_id=$1`, s.tenant).Scan(&head, &floor, &floorBoundary)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return page, err
	}
	position, boundary := floor, floorBoundary
	if query.After != "" {
		cursor, after, err := decodeEventCursor(query.After)
		if err != nil {
			return page, err
		}
		if cursor.Scope != s.tenant || !slices.Equal(cursor.Types, types) {
			return page, ErrEventCursorInvalid
		}
		if cursor.Epoch != epoch || after < floor || after > head {
			return page, ErrEventCursorExpired
		}
		position, boundary = after, cursor.Boundary
		want := floorBoundary
		if after > floor {
			if err := tx.QueryRow(ctx, `SELECT boundary::text FROM `+s.t.events+` WHERE tenant_id=$1 AND position=$2`, s.tenant, after).Scan(&want); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return page, ErrEventCursorExpired
				}
				return page, err
			}
		}
		if boundary != want {
			return page, ErrEventCursorExpired
		}
	}
	statement := `SELECT id::text,event_version,type,payload,created_at,position,boundary::text FROM ` + s.t.events + ` WHERE tenant_id=$1 AND position>$2 AND position<=$3`
	args := []any{s.tenant, position, head}
	if len(types) > 0 {
		statement += ` AND type=ANY($4) ORDER BY position LIMIT $5`
		args = append(args, types, limit+1)
	} else {
		statement += ` ORDER BY position LIMIT $4`
		args = append(args, limit+1)
	}
	rows, err := tx.Query(ctx, statement, args...)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var event EventRecord
		var payload []byte
		var next int64
		var token string
		if err := rows.Scan(&event.ID, &event.Version, &event.Type, &payload, &event.OccurredAt, &next, &token); err != nil {
			rows.Close()
			return page, err
		}
		var fact eventPayload
		if err := json.Unmarshal(payload, &fact); err != nil {
			rows.Close()
			return page, err
		}
		event.Kind, event.ContentID, event.VersionID = fact.Kind, fact.ContentID, fact.VersionID
		if len(page.Events) == limit {
			page.HasMore = true
			break
		}
		page.Events = append(page.Events, event)
		position, boundary = next, token
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return page, err
	}
	if !page.HasMore {
		position, boundary = head, floorBoundary
		if head > floor {
			if err := tx.QueryRow(ctx, `SELECT boundary::text FROM `+s.t.events+` WHERE tenant_id=$1 AND position=$2`, s.tenant, head).Scan(&boundary); err != nil {
				return page, err
			}
		}
	}
	page.NextCursor = encodeEventCursor(eventCursor{Version: 1, Scope: s.tenant, Epoch: epoch, Position: strconv.FormatInt(position, 10), Boundary: boundary, Types: types})
	if err := tx.Commit(ctx); err != nil {
		return EventPage{}, err
	}
	return page, nil
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
