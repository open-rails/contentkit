package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Event types written to the outbox.
const (
	EventContentCreated     = "content.created"
	EventContentUpdated     = "content.updated"
	EventContentScheduled   = "content.scheduled"
	EventContentPublished   = "content.published"
	EventContentUnpublished = "content.unpublished"
	EventContentDeleted     = "content.deleted"
	EventContentRestored    = "content.restored"
	EventContentPurged      = "content.purged"
	EventVersionCreated     = "version.created"
	EventVersionUpdated     = "version.updated"
	EventVersionPublished   = "version.published"
	EventVersionDeleted     = "version.deleted"
)

// Event is one outbox entry: a change to a content item or one of its
// versions, delivered at least once in order of ID.
type Event struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"`
	Kind      string    `json:"kind"`
	ContentID string    `json:"content_id"`
	VersionID string    `json:"version_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type eventPayload struct {
	Kind      string `json:"kind"`
	ContentID string `json:"content_id"`
	VersionID string `json:"version_id,omitempty"`
}

// Subscriber receives a batch of events in order. An error redelivers the
// batch later, so a subscriber must be idempotent.
type Subscriber func(ctx context.Context, events []Event) error

// Subscribe registers an in-process subscriber; DeliverEvents calls every
// subscriber with every batch.
func (s *Store) Subscribe(sub Subscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribers = append(s.subscribers, sub)
}

// emit writes events in q's transaction.
func (s *Store) emit(ctx context.Context, q Querier, events ...Event) error {
	if len(events) == 0 {
		return nil
	}
	types, entities, payloads := make([]string, len(events)), make([]string, len(events)), make([]string, len(events))
	for i, e := range events {
		p, err := json.Marshal(eventPayload{e.Kind, e.ContentID, e.VersionID})
		if err != nil {
			return err
		}
		entity := e.ContentID
		if e.VersionID != "" {
			entity = e.VersionID
		}
		types[i], entities[i], payloads[i] = e.Type, entity, string(p)
	}
	_, err := q.Exec(ctx, `INSERT INTO `+s.t.events+` (tenant_id, type, entity_id, payload)
 SELECT $1, t, e, p::jsonb FROM unnest($2::text[], $3::text[], $4::text[]) WITH ORDINALITY AS x(t, e, p, n) ORDER BY n`,
		s.tenant, types, entities, payloads)
	return err
}

// eventBatch bounds one delivery.
const eventBatch = 200

// deliveredRetention is how long delivered events stay in the outbox.
const deliveredRetention = 7 * 24 * time.Hour

// DeliverEvents hands undelivered events to the subscribers in batches, in
// order, and marks each batch delivered once every subscriber took it; it
// stops at the first failure, which it returns. Delivered events older than a
// week are pruned. It returns how many events were delivered.
func (s *Store) DeliverEvents(ctx context.Context) (int, error) {
	s.mu.Lock()
	subs := append([]Subscriber(nil), s.subscribers...)
	s.mu.Unlock()
	if _, err := s.pool.Exec(ctx, `DELETE FROM `+s.t.events+` WHERE tenant_id = $1 AND delivered_at < $2`, s.tenant, s.now().Add(-deliveredRetention)); err != nil {
		return 0, err
	}
	total := 0
	for {
		n, err := s.deliverBatch(ctx, subs)
		total += n
		if err != nil || n < eventBatch {
			return total, err
		}
	}
}

func (s *Store) deliverBatch(ctx context.Context, subs []Subscriber) (int, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id, type, payload, created_at FROM `+s.t.events+`
 WHERE tenant_id = $1 AND delivered_at IS NULL ORDER BY id LIMIT $2 FOR UPDATE SKIP LOCKED`, s.tenant, eventBatch)
	if err != nil {
		return 0, err
	}
	var batch []Event
	var ids []int64
	for rows.Next() {
		var e Event
		var raw []byte
		if err := rows.Scan(&e.ID, &e.Type, &raw, &e.CreatedAt); err != nil {
			rows.Close()
			return 0, err
		}
		var p eventPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			rows.Close()
			return 0, err
		}
		e.Kind, e.ContentID, e.VersionID = p.Kind, p.ContentID, p.VersionID
		batch, ids = append(batch, e), append(ids, e.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(batch) == 0 {
		return 0, err
	}
	for _, sub := range subs {
		if err := sub(ctx, batch); err != nil {
			return 0, errors.Join(errors.New("catalog: event subscriber failed"), err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE `+s.t.events+` SET delivered_at = clock_timestamp() WHERE tenant_id = $1 AND id = ANY($2)`, s.tenant, ids); err != nil {
		return 0, err
	}
	return len(batch), tx.Commit(ctx)
}
