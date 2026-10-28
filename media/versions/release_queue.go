package versions

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/media"
)

func (s *Service) releaseRequestsTable() string {
	return pgxIdent(s.o.Schema, "content_publication_requests")
}

// SchedulePublicationTx accepts current catalog/manifest intent in the same
// transaction as its runnable River obligation. Empty Version expands to the
// gallery's editions; each remains an independent request and access boundary.
func (s *Service) SchedulePublicationTx(ctx context.Context, tx pgx.Tx, targets []Target) error {
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
	resolved, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Target])
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

// wakePublicationTx resumes an accepted publication without the processor's clock.
// A processor observes readiness; it does not submit new publication intent.
// The request's stored deadline also keeps external workers independent of the
// host's clock. A frozen plan gets a successor at that same accepted deadline.
// Processing can change a released edition's pages, so a request parked at a
// later date moves back to when the live release completed and re-plans.
func (s *Service) wakePublicationTx(ctx context.Context, tx pgx.Tx, target Target) error {
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
		return s.SchedulePublicationTx(ctx, tx, []Target{target})
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
func (s *Service) scheduleReleaseAtTx(ctx context.Context, tx pgx.Tx, target Target, at time.Time) error {
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
	return media.InsertOnce(ctx, func(ctx context.Context, args river.JobArgs, options *river.InsertOpts) (*rivertype.JobInsertResult, error) {
		return client.InsertTx(ctx, tx, args, options)
	}, releaseArgs{ID: id, Due: at}, river.InsertOpts{Queue: QueueFor(s.o.Tenant), ScheduledAt: at})
}

type releaseArgs struct {
	ID    string    `json:"id"`
	Due   time.Time `json:"due"`
	After int64     `json:"after,omitempty"`
}

func (releaseArgs) Kind() string { return "contentkit_version_release" }
func (args releaseArgs) FollowUp(id int64) river.JobArgs {
	args.After = id
	return args
}

type releaseWorker struct {
	river.WorkerDefaults[releaseArgs]
	s *Service
}

func (s *Service) addReleaseWorker(workers *river.Workers) error {
	return river.AddWorkerSafely(workers, &releaseWorker{s: s})
}

func (w *releaseWorker) Timeout(*river.Job[releaseArgs]) time.Duration { return 30 * time.Minute }

func (w *releaseWorker) Work(ctx context.Context, job *river.Job[releaseArgs]) error {
	if err := media.WaitFor(ctx, river.ClientFromContext[pgx.Tx](ctx), job.Args.After); err != nil {
		return err
	}
	var due time.Time
	var current bool
	var state string
	err := w.s.o.Pool.QueryRow(ctx, `SELECT scheduled_at,current,state FROM `+w.s.releaseRequestsTable()+`
 WHERE tenant_id=$1 AND id=$2`, w.s.o.Tenant, job.Args.ID).Scan(&due, &current, &state)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && ((!current && state != "retiring") || state == "cancelled") {
		return river.JobCancel(errRetired)
	}
	if err != nil {
		return err
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
