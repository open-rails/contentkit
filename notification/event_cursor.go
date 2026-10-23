package notification

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/helpers/notify"
)

// ErrEventCursorChanged means another replica committed this source page first.
// Read the new cursor before trying again; never overwrite newer progress.
var ErrEventCursorChanged = errors.New("notification: event cursor changed")

// EventImportResult counts the committed inbox effects, including idempotent
// replays. Missing and erased recipients are deliberately skipped.
type EventImportResult struct{ Published, Skipped int }

// EventCursor is the inbox's progress through one trusted source. It is local
// destination state; reading it does not register anything with the producer.
func (s *Store) EventCursor(ctx context.Context, origin notify.OriginScope) (string, error) {
	if err := validEventOrigin(origin); err != nil {
		return "", err
	}
	var cursor string
	err := s.pool.QueryRow(ctx, `SELECT cursor FROM `+s.eventCursorsTable()+`
		WHERE namespace=$1 AND system=$2 AND origin_scope=$3`, s.namespace, origin.System, origin.Scope).Scan(&cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return cursor, err
}

// ImportEvents saves a source page and its continuation in the same transaction.
// Network reads and recipient mapping happen before this call. The row lock
// only covers local writes, so replicas cannot lose progress or partial pages.
func (s *Store) ImportEvents(ctx context.Context, origin notify.OriginScope, after, next string, inputs []notify.Input) (EventImportResult, error) {
	var result EventImportResult
	if err := validEventOrigin(origin); err != nil {
		return result, err
	}
	if next == "" || len(after) > 8192 || len(next) > 8192 || len(inputs) > 100 {
		return result, fmt.Errorf("notification: invalid event page")
	}
	for _, in := range inputs {
		if in.Origin.System != origin.System || in.Origin.Scope != origin.Scope {
			return result, fmt.Errorf("notification: event belongs to another origin")
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	table := s.eventCursorsTable()
	if _, err := tx.Exec(ctx, `INSERT INTO `+table+` (namespace,system,origin_scope,cursor)
		VALUES ($1,$2,$3,'') ON CONFLICT DO NOTHING`, s.namespace, origin.System, origin.Scope); err != nil {
		return result, err
	}
	var current string
	if err := tx.QueryRow(ctx, `SELECT cursor FROM `+table+`
		WHERE namespace=$1 AND system=$2 AND origin_scope=$3 FOR UPDATE`, s.namespace, origin.System, origin.Scope).Scan(&current); err != nil {
		return result, err
	}
	if current != after {
		return result, ErrEventCursorChanged
	}
	for _, in := range inputs {
		_, err := s.PublishTx(ctx, tx, in)
		if errors.Is(err, notify.ErrRecipientErased) || errors.Is(err, ErrRecipientNotFound) {
			result.Skipped++
			continue
		}
		if err != nil {
			return EventImportResult{}, err
		}
		result.Published++
	}
	// Quiet polls leave the row alone, avoiding a write every thirty seconds.
	if current != next {
		if _, err := tx.Exec(ctx, `UPDATE `+table+` SET cursor=$4,updated_at=clock_timestamp()
			WHERE namespace=$1 AND system=$2 AND origin_scope=$3`, s.namespace, origin.System, origin.Scope, next); err != nil {
			return EventImportResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return EventImportResult{}, err
	}
	return result, nil
}

func (s *Store) eventCursorsTable() string {
	return pgx.Identifier{s.schema, "notification_event_cursors"}.Sanitize()
}

func validEventOrigin(origin notify.OriginScope) error {
	for _, value := range []string{origin.System, origin.Scope} {
		if strings.TrimSpace(value) == "" || len(value) > 512 || strings.ContainsRune(value, 0) {
			return fmt.Errorf("notification: invalid event origin")
		}
	}
	return nil
}
