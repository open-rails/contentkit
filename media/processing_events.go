package media

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/helpers/eventlog"
	eventpg "github.com/open-rails/helpers/eventlog/postgres"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

const (
	EventProcessingProgressed = "media.processing_progressed"
	EventProcessingCompleted  = "media.processing_completed"
)

// ProcessingEvent is a durable observation of media, without delivery URLs or
// credentials. Completion includes settled failures; State distinguishes them.
type ProcessingEvent struct {
	Kind         string          `json:"kind"`
	ContentID    string          `json:"content_id"`
	VersionID    string          `json:"version_id,omitempty"`
	State        string          `json:"state"`
	Uploads      ProcessingCount `json:"uploads"`
	Archives     ArchiveStatus   `json:"archives"`
	Presets      []PresetStatus  `json:"presets"`
	FailureCount int             `json:"failure_count"`
	Playable     bool            `json:"playable"`
}

// CompleteProcessing commits any changed processor observation together with
// its idempotent metadata work. The caller must return an error to its processing job
// if this fails. Retrying re-reads current media under a per-item database lock;
// the durable fingerprint suppresses duplicate facts even after log retention.
func CompleteProcessing(ctx context.Context, manifests *Manifests, ref contentref.ContentRef, metadata func(context.Context, pgx.Tx, Readiness) error) error {
	if manifests == nil {
		return errors.New("media: processing completion needs Manifests")
	}
	journal := manifests.journal.forTenant(ref.TenantID)
	pool, schema := journal.pool, journal.schema
	item, err := manifests.Registry().Item(ref)
	if err != nil {
		return err
	}
	table := pgx.Identifier{schema, "content_media_processing"}.Sanitize()
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// Lock before reading S3, so a delayed processor cannot overwrite a newer
		// observation using a manifest it read before another completion committed.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, schema+"/media-processing/"+item.Prefix()); err != nil {
			return err
		}
		m, _, err := manifests.GetTx(ctx, tx, ref)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		k := item.Kind()
		status := statusOf(k, m, EncodeStatus{})
		fact := ProcessingEvent{Kind: ref.ContentKind, ContentID: ref.ContentID, VersionID: ref.Version(), State: status.State, Uploads: status.Uploads, Archives: status.Archives, Presets: status.Presets, FailureCount: status.FailureCount, Playable: k.Playable(m)}
		payload, err := json.Marshal(fact)
		if err != nil {
			return err
		}
		// Output identity matters even when counts are unchanged: replacing one page
		// or selecting a different cover must wake the metadata snapshot worker.
		identity := m.Clone()
		identity.Receipt = nil
		identity.Hidden = false
		identity.Meta = nil
		state, err := json.Marshal(struct {
			Manifest *Manifest
			Fact     ProcessingEvent
		}{identity, fact})
		if err != nil {
			return err
		}
		fingerprint := sha256.Sum256(state)
		var old []byte
		err = tx.QueryRow(ctx, `SELECT fingerprint FROM `+table+` WHERE tenant_id=$1 AND folder_prefix=$2`, ref.TenantID, item.Prefix()).Scan(&old)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		readiness := k.Readiness(m)
		// Event deduplication must not suppress an idempotent metadata repair.
		// A prior snapshot job may have failed after the observation committed.
		if metadata != nil {
			if err := metadata(ctx, tx, readiness); err != nil {
				return err
			}
		}
		if len(m.Files) == 0 && len(old) == 0 || string(old) == string(fingerprint[:]) {
			return nil
		}
		eventType := EventProcessingProgressed
		if status.State != StateProcessing {
			eventType = EventProcessingCompleted
		}
		scope, err := processingEventScope(ctx, tx, schema, ref.TenantID)
		if err != nil {
			return err
		}
		// Preserve catalog decimal IDs and its existing cursor/reader envelope.
		var id string
		if err := tx.QueryRow(ctx, `SELECT nextval($1::regclass)::text`, pgx.Identifier{schema, "content_event_ids"}.Sanitize()).Scan(&id); err != nil {
			return err
		}
		at := time.Now().UTC()
		log, err := eventpg.New(pool, eventpg.Config{Schema: schema, Source: "contentkit", Scope: scope})
		if err != nil {
			return err
		}
		if err := log.AppendTx(ctx, tx, eventlog.Event{ID: id, Source: "contentkit", Scope: scope, Type: eventType, Version: 1, OccurredAt: at, Subject: eventlog.Subject{Type: ref.ContentKind, ID: ref.ContentID}, Data: payload}); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO `+table+` (tenant_id,folder_prefix,fingerprint,observed_at) VALUES($1,$2,$3,$4)
ON CONFLICT(tenant_id,folder_prefix) DO UPDATE SET fingerprint=excluded.fingerprint,observed_at=excluded.observed_at`, ref.TenantID, item.Prefix(), fingerprint[:], at)
		return err
	})
}

func processingEventScope(ctx context.Context, tx pgx.Tx, schema, namespace string) (string, error) {
	scope, scoped := layout.ScopeFromPartition(namespace)
	if !scoped || scope.AppID == "" {
		return namespace, nil
	}
	var catalog string
	err := tx.QueryRow(ctx, `SELECT catalog_key FROM `+pgx.Identifier{schema, "content_app_bindings"}.Sanitize()+` WHERE tenant_id=$1::uuid AND app_id=$2::uuid`, scope.TenantID, scope.AppID).Scan(&catalog)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("media: processing event has no catalog binding for %s", namespace)
	}
	return catalog, err
}
