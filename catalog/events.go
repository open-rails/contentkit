package catalog

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
)

// Event types recorded with the domain mutation.
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
	EventVersionPurged      = "version.purged"
)

// Event is the catalog fact appended by a mutation. Readers receive EventRecord.
type Event struct {
	Type      string
	Kind      string
	ContentID string
	VersionID string
}

type eventPayload struct {
	Kind      string `json:"kind"`
	ContentID string `json:"content_id"`
	VersionID string `json:"version_id,omitempty"`
}

// emit allocates committed order under this app's row lock. The lock and rows
// commit with the mutation; a rollback publishes neither a gap nor an event.
func (s *Store) emit(ctx context.Context, tx pgx.Tx, events ...Event) error {
	if len(events) == 0 {
		return nil
	}
	if tx == nil {
		return errors.New("catalog: events require the mutation transaction")
	}
	var last int64
	if err := tx.QueryRow(ctx, `INSERT INTO `+s.qs+`.content_event_streams AS stream (tenant_id,position) VALUES ($1,$2)
ON CONFLICT (tenant_id) DO UPDATE SET position=stream.position+EXCLUDED.position RETURNING position`, s.tenant, len(events)).Scan(&last); err != nil {
		return err
	}
	types, entities, payloads := make([]string, len(events)), make([]string, len(events)), make([]string, len(events))
	for i, e := range events {
		payload, err := json.Marshal(eventPayload{e.Kind, e.ContentID, e.VersionID})
		if err != nil {
			return err
		}
		entity := e.ContentID
		if e.VersionID != "" {
			entity = e.VersionID
		}
		types[i], entities[i], payloads[i] = e.Type, entity, string(payload)
	}
	_, err := tx.Exec(ctx, `INSERT INTO `+s.t.events+` (tenant_id,position,type,entity_id,payload,created_at)
SELECT $1,$2+n,t,e,p::jsonb,clock_timestamp() FROM unnest($3::text[],$4::text[],$5::text[]) WITH ORDINALITY AS x(t,e,p,n) ORDER BY n`,
		s.tenant, last-int64(len(events)), types, entities, payloads)
	return err
}

// purgeTx is domain cleanup, independent of who reads the public event log.
func (s *Store) purgeTx(ctx context.Context, tx pgx.Tx, events []Event) error {
	if s.onPurge == nil || len(events) == 0 {
		return nil
	}
	refs := make([]contentref.ContentRef, 0, len(events))
	for _, event := range events {
		ref := s.Ref(event.Kind, event.ContentID)
		if event.VersionID != "" {
			ref = ref.WithVersion(event.VersionID)
		}
		refs = append(refs, ref)
	}
	return s.onPurge(ctx, tx, refs)
}
