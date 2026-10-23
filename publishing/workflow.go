package publishing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

// Complete accepts the bytes already uploaded by the caller. The exact commit
// ID and operation batch are retained, so a lost response or worker restart
// cannot import a second archive into this version.
func (s *Store) Complete(ctx context.Context, actor access.Actor, id string) (Operation, error) {
	r, err := s.owned(ctx, actor, id)
	if err != nil {
		return Operation{}, err
	}
	if r.State != AwaitingUpload {
		return r.Operation, nil
	}
	if r.plan == nil {
		return r.Operation, failure("not_uploaded", "archive", "prepare the upload before completing it")
	}
	lease := uuid.NewString()
	tag, err := s.o.Pool.Exec(ctx, `UPDATE `+s.table+` SET commit_started=true,lease_id=$3,lease_until=now()+interval '2 minutes' WHERE tenant_id=$1 AND id=$2 AND state='awaiting_upload' AND (lease_until IS NULL OR lease_until<now())`, s.o.Tenant, id, lease)
	if err != nil {
		return r.Operation, err
	}
	if tag.RowsAffected() == 0 {
		return r.Operation, &OperationError{Code: "operation_busy", Message: "operation is already being completed; retry", Retryable: true}
	}
	defer s.release(ctx, id, lease)
	// Re-read after reserving the transport; a concurrent successful Put may
	// have renewed it before this lease was acquired.
	r, err = s.load(ctx, s.o.Pool, id)
	if err != nil {
		return Operation{}, err
	}
	if r.plan == nil {
		_, _ = s.o.Pool.Exec(ctx, `UPDATE `+s.table+` SET commit_started=false WHERE tenant_id=$1 AND id=$2 AND lease_id=$3`, s.o.Tenant, id, lease)
		return r.Operation, &OperationError{Code: "not_uploaded", Message: "prepare the upload before completing it", Field: "archive", Retryable: true}
	}
	ref, err := s.ref(r)
	if err != nil {
		return r.Operation, err
	}
	ops := []media.Op{{Op: media.OpPut, Path: r.plan.Path, Blob: r.plan.Blob, Meta: map[string]any{versions.ArchiveOperation: id, versions.ArchiveSHA256: r.request.Archive.SHA256}}}
	_, err = s.o.Uploads.Commit(ctx, actor, ref, r.commitID, ops)
	missingBeforeComplete := false
	if ue, ok := media.AsUploadError(err); ok && ue.Code == media.CodeNotUploaded && r.plan.Multipart != nil {
		missingBeforeComplete = true
		_, err = s.o.Uploads.Complete(ctx, actor, r.plan.Multipart.Ticket)
		if err == nil {
			_, err = s.o.Uploads.Commit(ctx, actor, ref, r.commitID, ops)
		}
	}
	if err != nil {
		if ue, ok := media.AsUploadError(err); ok && (ue.Code == media.CodeNotUploaded || missingBeforeComplete && (ue.Code == media.CodeNotFound || ue.Code == media.CodeInvalid)) {
			// No staged bytes were committed. Release a failed/expired plan so
			// Put can supply a fresh one, with a distinct commit fingerprint.
			if missingBeforeComplete && (ue.Code == media.CodeNotFound || ue.Code == media.CodeInvalid) || r.expires == nil || !r.expires.After(s.o.Clock()) {
				_, _ = s.o.Pool.Exec(ctx, `UPDATE `+s.table+` SET commit_started=false,upload=NULL,upload_expires_at=NULL,commit_id=$4,updated_at=now() WHERE tenant_id=$1 AND id=$2 AND lease_id=$3`, s.o.Tenant, id, lease, uuid.NewString())
			} else {
				_, _ = s.o.Pool.Exec(ctx, `UPDATE `+s.table+` SET commit_started=false WHERE tenant_id=$1 AND id=$2 AND lease_id=$3`, s.o.Tenant, id, lease)
			}
			return r.Operation, &OperationError{Code: "not_uploaded", Message: "upload is missing or expired; resume this operation to obtain upload instructions", Field: "archive", Retryable: true}
		}
		return r.Operation, err
	}
	tx, err := s.o.Pool.Begin(ctx)
	if err != nil {
		return r.Operation, err
	}
	defer tx.Rollback(ctx)
	tag, err = tx.Exec(ctx, `UPDATE `+s.table+` SET state='processing',progress='{"stage":"importing","completed":0,"total":0}',updated_at=now() WHERE tenant_id=$1 AND id=$2 AND lease_id=$3`, s.o.Tenant, id, lease)
	if err != nil {
		return r.Operation, err
	}
	if tag.RowsAffected() != 1 {
		return r.Operation, &OperationError{Code: "operation_busy", Message: "operation lease changed while completing; retry", Retryable: true}
	}
	if err = s.enqueueOperation(ctx, tx, id); err != nil {
		return r.Operation, err
	}
	if err = tx.Commit(ctx); err != nil {
		return r.Operation, err
	}
	r, err = s.load(ctx, s.o.Pool, id)
	return r.Operation, err
}

func (s *Store) release(ctx context.Context, id, lease string) {
	clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = s.o.Pool.Exec(clean, `UPDATE `+s.table+` SET lease_id=NULL,lease_until=NULL WHERE tenant_id=$1 AND id=$2 AND lease_id=$3`, s.o.Tenant, id, lease)
}

// Reconcile queues monitors for a bounded batch independently of HTTP traffic.
// Without a bound fleet it advances the batch inline. Repeating it after a
// crash is safe; River's periodic contribution supplies recovery.
func (s *Store) Reconcile(ctx context.Context) error {
	rows, err := s.o.Pool.Query(ctx, `SELECT operations.id::text FROM `+s.table+` operations WHERE tenant_id=$1 AND (state IN ('processing','publishing','review_required') OR state='blocked' AND failure->>'retryable'='true' OR state='scheduled' AND publish_at<=$2) AND (lease_until IS NULL OR lease_until<now()) ORDER BY operations.reconcile_at,operations.id LIMIT 25`, s.o.Tenant, s.o.Clock())
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	var all error
	for _, id := range ids {
		var err error
		if s.client() != nil {
			err = s.recoverOperation(ctx, id)
		} else {
			err = s.reconcile(ctx, id)
		}
		if err != nil {
			s.o.Logger.WarnContext(ctx, "publishing operation will retry", "operation", id, "error", err)
			all = errors.Join(all, err)
		}
	}
	return all
}

func (s *Store) reconcile(ctx context.Context, id string) error {
	lease := uuid.NewString()
	// Rotate at claim time so errors, no-op waits and interrupted attempts do
	// not hold the front of the queue. Keep user-visible updated_at unchanged.
	tag, err := s.o.Pool.Exec(ctx, `UPDATE `+s.table+` SET lease_id=$3,lease_until=now()+interval '2 minutes',reconcile_at=now() WHERE tenant_id=$1 AND id=$2 AND (lease_until IS NULL OR lease_until<now())`, s.o.Tenant, id, lease)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	defer s.release(ctx, id, lease)
	r, err := s.load(ctx, s.o.Pool, id)
	if err != nil {
		return err
	}
	if r.State != Processing && r.State != Publishing && r.State != Scheduled && r.State != ReviewRequired && r.State != Blocked {
		return nil
	}
	var state, moderation string
	var cr, vr int64
	var live bool
	err = s.o.Pool.QueryRow(ctx, `SELECT c.state,c.moderation,c.revision,v.revision,c.published_at IS NOT NULL FROM `+s.items+` c JOIN `+s.versions+` v ON v.tenant_id=c.tenant_id AND v.content_id=c.id AND v.id=$3 WHERE c.tenant_id=$1 AND c.id=$2`, s.o.Tenant, r.ContentID, r.VersionID).Scan(&state, &moderation, &cr, &vr, &live)
	if errors.Is(err, pgx.ErrNoRows) || state == catalog.StateDeleted {
		return s.fail(ctx, r, lease, Blocked, failure("content_changed", "content", "draft was removed while processing"))
	}
	if err != nil {
		return err
	}
	if r.State == ReviewRequired || r.State == Publishing || r.State == Scheduled {
		switch {
		case state == catalog.StatePublished && moderation == content.ModerationApproved && live:
			ready, err := s.o.Versions.ArchiveStatus(ctx, versions.Target{Content: r.ContentID, Version: r.VersionID}, id, r.request.Archive.SHA256)
			if errors.Is(err, versions.ErrArchiveChanged) || errors.Is(err, versions.ErrArchiveFailed) || errors.Is(err, versions.ErrNotFound) || errors.Is(err, media.ErrNotFound) {
				return s.fail(ctx, r, lease, Blocked, failure("content_changed", "archive", "media changed while publication was being exposed"))
			}
			if err != nil {
				return err
			}
			if ready.PublicReady {
				return s.finish(ctx, r, lease, Published, ready.Pages)
			}
			_, err = s.o.Pool.Exec(ctx, `UPDATE `+s.table+` SET state='publishing',progress=$4,updated_at=now() WHERE tenant_id=$1 AND id=$2 AND lease_id=$3`, s.o.Tenant, id, lease, jsonBytes(Progress{Stage: "exposing", Completed: ready.Completed, Total: ready.Pages}))
			return err
		case state == catalog.StatePublished && moderation == content.ModerationApproved && !live:
			if r.request.PublishAt != nil && r.request.PublishAt.After(s.o.Clock()) {
				if _, err := s.o.Versions.ArchiveStatus(ctx, versions.Target{Content: r.ContentID, Version: r.VersionID}, id, r.request.Archive.SHA256); err != nil {
					return err
				}
				return s.finish(ctx, r, lease, Scheduled, r.Progress.Total)
			}
			// The catalog's ordinary due-publication job owns activation and
			// exposure. A due operation waits without changing its intended date.
			return nil
		case moderation == content.ModerationRejected:
			return s.fail(ctx, r, lease, Blocked, failure("moderation_rejected", "content", "publication was rejected by moderation"))
		case moderation == content.ModerationHeld:
			_, err := s.o.Pool.Exec(ctx, `UPDATE `+s.table+` SET state='review_required',updated_at=now() WHERE tenant_id=$1 AND id=$2 AND lease_id=$3`, s.o.Tenant, id, lease)
			return err
		default:
			return s.fail(ctx, r, lease, Blocked, failure("content_changed", "content", "publication was changed while awaiting review"))
		}
	}
	// A crash after catalog commit is recognized by the atomic state change in
	// PublishChecked below; no generic published item is adopted as success.
	if cr != r.contentRevision || vr != r.versionRevision || state != catalog.StateDraft {
		return s.fail(ctx, r, lease, Blocked, failure("content_changed", "content", "draft changed while processing; publish the edited draft explicitly"))
	}
	target := versions.Target{Content: r.ContentID, Version: r.VersionID}
	ready, err := s.o.Versions.ArchiveStatus(ctx, target, id, r.request.Archive.SHA256)
	if err != nil {
		if errors.Is(err, versions.ErrNotFound) || errors.Is(err, media.ErrNotFound) {
			return s.fail(ctx, r, lease, Blocked, failure("content_changed", "archive", "submitted media was removed while processing"))
		}
		if errors.Is(err, versions.ErrArchiveChanged) {
			return s.fail(ctx, r, lease, Blocked, failure("content_changed", "archive", "imported pages no longer match the submitted archive"))
		}
		if errors.Is(err, versions.ErrArchiveFailed) {
			return s.fail(ctx, r, lease, Failed, failure("invalid_archive", "archive", "archive or required media could not be processed"))
		}
		return err
	}
	r.Progress = Progress{Stage: "rendering", Completed: ready.Completed, Total: ready.Pages}
	if err = s.progress(ctx, r, lease); err != nil {
		return err
	}
	if !ready.Ready {
		return nil
	}
	if r.request.Publish == Draft {
		return s.finish(ctx, r, lease, Ready, ready.Pages)
	}
	if s.o.Authority == nil {
		return s.fail(ctx, r, lease, Blocked, failure("forbidden", "publish", "background publication authority is unavailable"))
	}
	actor, err := s.o.Authority.Resume(ctx, r.authority)
	if err == nil {
		err = s.allowed(ctx, actor, access.ContentWrite)
	}
	if err != nil {
		f := failure("forbidden", "publish", "publication authority was revoked or is no longer sufficient")
		if errors.Is(err, auth.ErrUnavailable) {
			f.Code = "unavailable"
			f.Message = "publication authority is temporarily unavailable"
			f.Retryable = true
		}
		return s.fail(ctx, r, lease, Blocked, f)
	}
	err = s.o.Versions.WithArchiveReady(ctx, target, id, r.request.Archive.SHA256, func(ready versions.ArchiveReadiness) error {
		return s.o.Catalog.PublishChecked(ctx, actor, []catalog.Publish{{ContentID: r.ContentID, At: r.request.PublishAt}}, func(ctx context.Context, tx pgx.Tx) error {
			if err := s.checkRevision(ctx, tx, r); err != nil {
				return err
			}
			// The operation's publication intent is recorded in the same SQL
			// transaction as the catalog publication. A crash cannot lose it.
			tag, err := tx.Exec(ctx, `UPDATE `+s.table+` SET state='publishing',progress=$4,updated_at=now() WHERE tenant_id=$1 AND id=$2 AND lease_id=$3`, s.o.Tenant, id, lease, jsonBytes(Progress{Stage: "publication", Completed: ready.Pages, Total: ready.Pages}))
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return &OperationError{Code: "operation_busy", Message: "operation lease changed before publication; retry", Retryable: true}
			}
			return nil
		})
	})
	if errors.Is(err, versions.ErrNotReady) {
		return nil
	}
	var changed *OperationError
	if errors.As(err, &changed) && changed.Code == "content_changed" {
		return s.fail(ctx, r, lease, Blocked, changed)
	}
	var rejected content.RejectedError
	if errors.As(err, &rejected) {
		return s.fail(ctx, r, lease, Blocked, failure("moderation_rejected", "content", rejected.Reason))
	}
	if errors.Is(err, versions.ErrArchiveChanged) {
		return s.fail(ctx, r, lease, Blocked, failure("content_changed", "archive", "archive changed before publication"))
	}
	return err
}

func (s *Store) checkRevision(ctx context.Context, tx pgx.Tx, r record) error {
	var cr, vr int64
	var state string
	if err := tx.QueryRow(ctx, `SELECT c.revision,v.revision,c.state FROM `+s.items+` c JOIN `+s.versions+` v ON v.tenant_id=c.tenant_id AND v.content_id=c.id AND v.id=$3 WHERE c.tenant_id=$1 AND c.id=$2 FOR UPDATE OF v`, s.o.Tenant, r.ContentID, r.VersionID).Scan(&cr, &vr, &state); err != nil {
		return err
	}
	if cr != r.contentRevision || vr != r.versionRevision || state != catalog.StateDraft {
		return failure("content_changed", "content", "draft changed before publication")
	}
	rows, err := tx.Query(ctx, `SELECT entity_id FROM `+pgx.Identifier{s.o.Schema, "content_memberships"}.Sanitize()+` WHERE tenant_id=$1 AND content_id=$2 AND version_id IS NULL AND state='active' ORDER BY entity_id`, s.o.Tenant, r.ContentID)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	expected := slices.Clone(r.entityIDs)
	slices.Sort(expected)
	if !slices.Equal(ids, expected) {
		return failure("content_changed", "entities", "draft entities changed before publication")
	}
	return nil
}

func (s *Store) progress(ctx context.Context, r record, lease string) error {
	_, err := s.o.Pool.Exec(ctx, `UPDATE `+s.table+` SET progress=$4,updated_at=now(),failure=NULL,state='processing' WHERE tenant_id=$1 AND id=$2 AND lease_id=$3`, s.o.Tenant, r.ID, lease, jsonBytes(r.Progress))
	return err
}

func (s *Store) fail(ctx context.Context, r record, lease string, state State, f *OperationError) error {
	_, err := s.o.Pool.Exec(ctx, `UPDATE `+s.table+` SET state=$4,failure=$5,updated_at=now() WHERE tenant_id=$1 AND id=$2 AND lease_id=$3`, s.o.Tenant, r.ID, lease, state, jsonBytes(f))
	return err
}

func (s *Store) finish(ctx context.Context, r record, lease string, state State, pages int) error {
	viewer, err := s.o.Catalog.Viewer(ctx, catalog.ViewerOptions{Actor: gate.Host()})
	if err != nil {
		return err
	}
	contents, err := s.o.Catalog.Contents(ctx, viewer, []string{r.ContentID})
	if err != nil {
		return err
	}
	if len(contents) == 0 || contents[0] == nil {
		return fmt.Errorf("publishing: completed content disappeared")
	}
	stage := "complete"
	if state == Scheduled {
		stage = "scheduled"
	}
	_, err = s.o.Pool.Exec(ctx, `UPDATE `+s.table+` SET state=$4,result=$5,progress=$6,failure=NULL,updated_at=now() WHERE tenant_id=$1 AND id=$2 AND lease_id=$3`, s.o.Tenant, r.ID, lease, state, jsonBytes(OperationResult{Path: contents[0].Path, Pages: pages}), jsonBytes(Progress{Stage: stage, Completed: pages, Total: pages}))
	return err
}

func jsonBytes(v any) []byte { b, _ := json.Marshal(v); return b }
