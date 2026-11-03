package versionstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/media"
	versions "github.com/open-rails/contentkit/media/versions"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

func (s *Service) releaseRequestsTable() string {
	return pgxIdent(s.o.Schema, "content_publication_requests")
}

// SchedulePublicationTx accepts current catalog/manifest intent in the same
// transaction as its runnable River obligation. Empty Version expands to the
// gallery's editions; each remains an independent request and access boundary.
func (s *Service) SchedulePublicationTx(ctx context.Context, tx pgx.Tx, targets []versions.Target) error {
	if tx == nil {
		return errors.New("versions: publication scheduling requires a transaction")
	}
	contents, vers := make([]string, len(targets)), make([]string, len(targets))
	for i, target := range targets {
		contents[i], vers[i] = target.Content, target.Version
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT v.content_id,v.id FROM `+s.t.versions+` v
 JOIN unnest($2::text[],$3::text[]) AS target(content_id,version_id)
 ON target.content_id=v.content_id AND (target.version_id='' OR target.version_id=v.id)
 WHERE v.tenant_id=$1 ORDER BY v.content_id,v.id`, s.o.Tenant, contents, vers)
	if err != nil {
		return err
	}
	resolved, err := pgx.CollectRows(rows, pgx.RowToStructByPos[versions.Target])
	if err != nil {
		return err
	}
	// Catalog writers hold parent/version rows before touching requests.
	// Metadata scheduling enters with no catalog locks, so acquire every
	// resolved target in the same deterministic order before any request.
	for _, target := range resolved {
		if err := s.lockTarget(ctx, tx, target); err != nil {
			return err
		}
	}
	for _, target := range resolved {
		at, err := s.releaseDueTx(ctx, tx, target)
		if err != nil {
			return err
		}
		// A zero due time means this draft has no released bytes to withdraw
		// and no accepted publication intent. It needs no request or job.
		if at.IsZero() {
			continue
		}
		if err := s.scheduleReleaseAtTx(ctx, tx, target, at); err != nil {
			return err
		}
	}
	return nil
}

// ReplanMissingUploads re-plans released versions whose live release lacks a
// required upload, which withdraws them. client only inserts the jobs.
func (s *Service) ReplanMissingUploads(ctx context.Context, client *river.Client[pgx.Tx]) (int, error) {
	if s.reg == nil {
		return 0, nil
	}
	var kinds []string
	for kind := range s.o.Kinds.Kinds {
		k, err := s.releaseMedia(kind)
		if err != nil {
			return 0, err
		}
		if k != nil && slices.ContainsFunc(k.Uploads, func(u media.Upload) bool { return u.Required }) {
			kinds = append(kinds, kind)
		}
	}
	if len(kinds) == 0 {
		return 0, nil
	}
	scheduler := publicationService(s.o.Pool, s.o.Schema, s.o.Tenant, s.reg)
	scheduler.o.Now, scheduler.client = s.o.Now, client
	const batch = 100
	replanned, after := 0, ""
	for {
		rows, err := s.o.Pool.Query(ctx, `SELECT v.content_id,v.id,c.kind,coalesce(jsonb_path_query_array(r.result,'$.view.files[*] ? (!exists(@.preset)).path'),'[]')
 FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 JOIN `+s.releaseRequestsTable()+` r ON r.tenant_id=v.tenant_id AND r.id=v.release_id AND r.state IN ('retiring','completed')
 WHERE v.tenant_id=$1 AND v.released AND v.state<>'deleted' AND c.state<>'deleted' AND c.kind=ANY($2) AND v.id>$3 ORDER BY v.id LIMIT `+strconv.Itoa(batch), s.o.Tenant, kinds, after)
		if err != nil {
			return replanned, err
		}
		var targets []versions.Target
		n := 0
		for rows.Next() {
			var t versions.Target
			var kind string
			var paths []string
			if err := rows.Scan(&t.Content, &t.Version, &kind, &paths); err != nil {
				rows.Close()
				return replanned, err
			}
			n, after = n+1, t.Version
			k, err := s.reg.Kind(kind)
			if err != nil {
				rows.Close()
				return replanned, err
			}
			view := &mediastore.Manifest{}
			for _, path := range paths {
				view.Files = append(view.Files, media.File{Path: path})
			}
			if len(k.Missing(view)) > 0 {
				targets = append(targets, t)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return replanned, err
		}
		if len(targets) > 0 {
			if err := pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error { return scheduler.SchedulePublicationTx(ctx, tx, targets) }); err != nil {
				return replanned, err
			}
			replanned += len(targets)
		}
		if n < batch {
			return replanned, nil
		}
	}
}

// wakePublicationTx resumes an accepted publication without the processor's clock.
// A processor observes readiness; it does not submit new publication intent.
// The request's stored deadline also keeps external workers independent of the
// host's clock. A frozen plan gets a successor at that same accepted deadline.
// Processing can change a released edition's pages, so a request parked at a
// later date moves back to when the live release completed and re-plans.
func (s *Service) wakePublicationTx(ctx context.Context, tx pgx.Tx, target versions.Target) error {
	if tx == nil {
		return errors.New("versions: publication wakeup requires a transaction")
	}
	if err := s.lockTarget(ctx, tx, target); err != nil {
		return err
	}
	var at time.Time
	var completed *time.Time
	err := tx.QueryRow(ctx, `SELECT r.scheduled_at,CASE WHEN v.released THEN live.completed_at END
 FROM `+s.releaseRequestsTable()+` r JOIN `+s.t.versions+` v ON v.tenant_id=r.tenant_id AND v.id=r.version_id
 LEFT JOIN `+s.releaseRequestsTable()+` live ON live.tenant_id=v.tenant_id AND live.id=v.release_id
 WHERE r.tenant_id=$1 AND r.content_id=$2 AND r.version_id=$3 AND r.current AND r.state<>'cancelled' FOR UPDATE OF r`,
		s.o.Tenant, target.Content, target.Version).Scan(&at, &completed)
	if errors.Is(err, pgx.ErrNoRows) {
		// The normal intent check leaves an unpublished draft without a job.
		return s.SchedulePublicationTx(ctx, tx, []versions.Target{target})
	}
	if err != nil {
		return err
	}
	if completed != nil && completed.Before(at) {
		at = *completed
	}
	return s.scheduleReleaseAtTx(ctx, tx, target, at)
}

// scheduleReleaseAtTx also schedules the next chapter boundary after a release.
// An unfrozen waiting request can follow new manifest order. Once frozen, its
// plan is immutable and another request owns later changes.
func (s *Service) scheduleReleaseAtTx(ctx context.Context, tx pgx.Tx, target versions.Target, at time.Time) error {
	if tx == nil || target.Content == "" || target.Version == "" || at.IsZero() {
		return errors.New("versions: publication needs a transaction, edition and due time")
	}
	if err := s.lockTarget(ctx, tx, target); err != nil {
		return err
	}
	client, err := s.bound()
	if err != nil {
		return err
	}
	at = at.UTC()
	var id string
	for {
		var currentID, state string
		var frozen bool
		err := tx.QueryRow(ctx, `SELECT id::text,state,plan IS NOT NULL FROM `+s.releaseRequestsTable()+`
 WHERE tenant_id=$1 AND content_id=$2 AND version_id=$3 AND current FOR UPDATE`,
			s.o.Tenant, target.Content, target.Version).Scan(&currentID, &state, &frozen)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && !frozen && (state == "pending" || state == "waiting") {
			id = currentID
			if _, err := tx.Exec(ctx, `UPDATE `+s.releaseRequestsTable()+` SET scheduled_at=$3,state='pending',error=NULL,updated_at=clock_timestamp()
 WHERE tenant_id=$1 AND id=$2`, s.o.Tenant, id, at); err != nil {
				return err
			}
			break
		}
		if err == nil {
			if _, err := tx.Exec(ctx, `UPDATE `+s.releaseRequestsTable()+` SET current=false,updated_at=clock_timestamp()
 WHERE tenant_id=$1 AND id=$2`, s.o.Tenant, currentID); err != nil {
				return err
			}
			if err := s.pruneRequestsTx(ctx, tx, target, false); err != nil {
				return err
			}
		}
		id = uuid.NewString()
		err = tx.QueryRow(ctx, `INSERT INTO `+s.releaseRequestsTable()+` (tenant_id,id,content_id,version_id,scheduled_at)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT(tenant_id,version_id) WHERE current DO NOTHING RETURNING id::text`,
			s.o.Tenant, id, target.Content, target.Version, at).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			// Another accepting transaction created the first current request.
			// Re-read it under the row lock; no second active owner is created.
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	return mediastore.InsertOnce(ctx, func(ctx context.Context, args river.JobArgs, options *river.InsertOpts) (*rivertype.JobInsertResult, error) {
		return client.InsertTx(ctx, tx, args, options)
	}, publicationArgs{ID: id, Due: at}, river.InsertOpts{Queue: QueueFor(s.o.Tenant), ScheduledAt: at})
}

// errRequestGone cancels a job whose request a newer release pruned.
var errRequestGone = fmt.Errorf("%w: publication request is gone", ErrNotFound)

// pruneRequestsTx deletes an edition's requests that nothing reads: not
// current, not its live release and not retiring. Readers use the live
// release, retirement uses retiring rows and media cleanup uses the manifest.
// Until a release completes, a superseded unfinished request's plan records
// that storage work started (releaseDueTx), so only completed ones go.
func (s *Service) pruneRequestsTx(ctx context.Context, tx pgx.Tx, target versions.Target, released bool) error {
	_, err := tx.Exec(ctx, `DELETE FROM `+s.releaseRequestsTable()+` r
 WHERE r.tenant_id=$1 AND r.version_id=$2 AND NOT r.current AND (r.state='completed' OR $3 AND r.state<>'retiring')
 AND r.id IS DISTINCT FROM (SELECT release_id FROM `+s.t.versions+` WHERE tenant_id=$1 AND id=$2)`, s.o.Tenant, target.Version, released)
	return err
}

type publicationArgs struct {
	ID    string    `json:"id"`
	Due   time.Time `json:"due"`
	After int64     `json:"after,omitempty"`
}

func (publicationArgs) Kind() string { return "contentkit_publication" }

func (args publicationArgs) FollowUp(id int64) river.JobArgs {
	args.After = id
	return args
}

type publicationWorker struct {
	river.WorkerDefaults[publicationArgs]
	s *Service
}

func (s *Service) addPublicationWorker(workers *river.Workers) error {
	return river.AddWorkerSafely(workers, &publicationWorker{s: s})
}

func (w *publicationWorker) Timeout(*river.Job[publicationArgs]) time.Duration {
	return 30 * time.Minute
}

func (w *publicationWorker) Work(ctx context.Context, job *river.Job[publicationArgs]) error {
	if err := mediastore.WaitFor(ctx, river.ClientFromContext[pgx.Tx](ctx), job.Args.After); err != nil {
		return err
	}
	var due time.Time
	var current bool
	var state string
	var target versions.Target
	err := w.s.o.Pool.QueryRow(ctx, `SELECT scheduled_at,current,state,content_id,version_id FROM `+w.s.releaseRequestsTable()+`
 WHERE tenant_id=$1 AND id=$2`, w.s.o.Tenant, job.Args.ID).Scan(&due, &current, &state, &target.Content, &target.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return river.JobCancel(errRetired)
	}
	if err != nil {
		return err
	}
	// Deletion and restore commit with this job: repeat their fence sync
	// even when newer intent has superseded the request.
	if err := w.s.SyncFences(ctx, []versions.Target{target}); err != nil {
		return err
	}
	if (!current && state != "retiring") || state == "cancelled" {
		return river.JobCancel(errRetired)
	}
	if state == "completed" {
		return nil
	}
	if wait := due.Sub(w.s.o.Now()); state != "retiring" && wait > 0 {
		return river.JobSnooze(wait)
	}
	err = w.s.runRelease(ctx, job.Args.ID)
	if errors.Is(err, ErrNotReady) || errors.Is(err, errRetired) {
		// Processing completion or newer intent owns the next wakeup; no
		// five-second readiness polling or repeated stale copy attempt.
		return nil
	}
	if errors.Is(err, ErrNotFound) {
		return river.JobCancel(err)
	}
	return err
}
