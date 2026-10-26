package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/open-rails/contentkit/access"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
)

// Event types recorded with the domain mutation.
const (
	EventContentCreated       = "content.created"
	EventContentUpdated       = "content.updated"
	EventContentScheduled     = "content.scheduled"
	EventContentPublished     = "content.published"
	EventContentUnpublished   = "content.unpublished"
	EventContentDeleted       = "content.deleted"
	EventContentRestored      = "content.restored"
	EventContentPurged        = "content.purged"
	EventVersionCreated       = "version.created"
	EventVersionUpdated       = "version.updated"
	EventVersionPublished     = "version.published"
	EventVersionDeleted       = "version.deleted"
	EventVersionPurged        = "version.purged"
	EventAudienceReleased     = "version.audience_released"
	EventPublicationCompleted = "version.publication_completed"
)

// Event is the catalog fact appended by a mutation. Readers receive EventRecord.
type Event struct {
	ChannelID     string
	Type          string
	Kind          string
	ContentID     string
	VersionID     string
	OccurredAt    time.Time
	Release       *access.Release
	PublicationID string
	ChapterIDs    []string
	Released      *bool
}

type eventPayload struct {
	ChannelID     string          `json:"channel_id,omitempty"`
	Kind          string          `json:"kind"`
	ContentID     string          `json:"content_id"`
	VersionID     string          `json:"version_id,omitempty"`
	Release       *access.Release `json:"release,omitempty"`
	PublicationID string          `json:"publication_id,omitempty"`
	ChapterIDs    []string        `json:"chapter_ids,omitempty"`
	Released      *bool           `json:"released,omitempty"`
}

// emit queues facts in the mutation transaction. The shared deferred flush
// locks all touched scopes in sorted order and assigns committed positions.
func (s *Store) emit(ctx context.Context, tx pgx.Tx, events ...Event) error {
	if len(events) == 0 {
		return nil
	}
	if tx == nil {
		return errors.New("catalog: events require the mutation transaction")
	}
	types, kinds, subjects, payloads := make([]string, len(events)), make([]string, len(events)), make([]string, len(events)), make([]string, len(events))
	times := make([]time.Time, len(events))
	for i, e := range events {
		times[i] = e.OccurredAt
		if times[i].IsZero() {
			times[i] = s.now()
		}
		payload, err := json.Marshal(eventPayload{ChannelID: e.ChannelID, Kind: e.Kind, ContentID: e.ContentID, VersionID: e.VersionID, Release: e.Release,
			PublicationID: e.PublicationID, ChapterIDs: e.ChapterIDs, Released: e.Released})
		if err != nil {
			return err
		}
		types[i], kinds[i], subjects[i], payloads[i] = e.Type, e.Kind, e.ContentID, string(payload)
	}
	// IDs remain decimal product identifiers. Their allocation is deliberately
	// independent of committed positions, which the shared flush assigns.
	_, err := tx.Exec(ctx, `INSERT INTO `+s.qs+`.eventlog_pending
 (source,scope,id,boundary,event_source,event_type,version,occurred_at,subject_type,subject_id,data)
 SELECT 'contentkit',$1,nextval($2::regclass)::text,gen_random_uuid()::text,'contentkit',t,1,at,k,c,p::jsonb
 FROM unnest($3::text[],$4::text[],$5::text[],$6::text[],$7::timestamptz[]) WITH ORDINALITY AS x(t,k,c,p,at,n) ORDER BY n`,
		s.tenant, s.qs+`.content_event_ids`, types, kinds, subjects, payloads, times)
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
