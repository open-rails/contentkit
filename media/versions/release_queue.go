package versions

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/media"
)

// ReleaseRequest is one durable reconciliation attempt for an edition. Plan
// freezes only when execution starts. A newer current request never replaces
// the completed reader snapshot referenced by content_versions.release_id.
type ReleaseRequest struct {
	ID string
	Target
	ScheduledAt time.Time
	State       string
	Current     bool
	Plan        json.RawMessage
	Result      json.RawMessage
	Error       string
	CompletedAt *time.Time
}

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
	contents, versions := make([]string, len(targets)), make([]string, len(targets))
	for i, target := range targets {
		contents[i], versions[i] = target.Content, target.Version
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT v.content_id,v.id FROM `+s.t.versions+` v
 JOIN unnest($2::text[],$3::text[]) AS target(content_id,version_id)
 ON target.content_id=v.content_id AND (target.version_id='' OR target.version_id=v.id)
 WHERE v.tenant_id=$1 ORDER BY v.content_id,v.id`, s.o.Tenant, contents, versions)
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

// wakePublicationTx resumes an accepted publication without changing its date.
// A processor observes readiness; it does not submit new publication intent.
// The request's stored deadline also keeps external workers independent of the
// host's clock. A frozen plan gets a successor at that same accepted deadline.
func (s *Service) wakePublicationTx(ctx context.Context, tx pgx.Tx, target Target) error {
	if tx == nil {
		return errors.New("versions: publication wakeup requires a transaction")
	}
	if err := s.lockTarget(ctx, tx, target); err != nil {
		return err
	}
	var at time.Time
	err := tx.QueryRow(ctx, `SELECT scheduled_at FROM `+s.releaseRequestsTable()+`
 WHERE tenant_id=$1 AND content_id=$2 AND version_id=$3 AND current AND state<>'cancelled' FOR UPDATE`,
		s.o.Tenant, target.Content, target.Version).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		// The normal intent check leaves an unpublished draft without a job.
		return s.SchedulePublicationTx(ctx, tx, []Target{target})
	}
	if err != nil {
		return err
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

// releaseRequestTx locks one exact request. It returns pgx.ErrNoRows when the
// request is absent in this tenant, including requests from another app.
func (s *Service) releaseRequestTx(ctx context.Context, tx pgx.Tx, id string) (*ReleaseRequest, error) {
	request := &ReleaseRequest{}
	err := tx.QueryRow(ctx, `SELECT id::text,content_id,version_id,scheduled_at,state,current,plan,result,coalesce(error,''),completed_at
 FROM `+s.releaseRequestsTable()+` WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, s.o.Tenant, id).
		Scan(&request.ID, &request.Content, &request.Version, &request.ScheduledAt, &request.State, &request.Current,
			&request.Plan, &request.Result, &request.Error, &request.CompletedAt)
	if err != nil {
		return nil, err
	}
	return request, nil
}

// setReleasePlanTx freezes the immutable input of a copy attempt. Retrying the
// same plan is safe; changing a frozen plan requires a new request.
func (s *Service) setReleasePlanTx(ctx context.Context, tx pgx.Tx, id string, plan json.RawMessage) error {
	if len(plan) == 0 || !json.Valid(plan) {
		return ErrInvalid
	}
	tag, err := tx.Exec(ctx, `UPDATE `+s.releaseRequestsTable()+` SET plan=coalesce(plan,$3::jsonb),state='running',error=NULL,updated_at=clock_timestamp()
 WHERE tenant_id=$1 AND id=$2 AND current AND state NOT IN ('retiring','completed','cancelled') AND (plan IS NULL OR plan=$3::jsonb)`, s.o.Tenant, id, []byte(plan))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errRetired
	}
	return nil
}

// finishReleaseTx commits a verified result while the request is still current.
// The caller updates reader snapshots, release_id and catalog state in this
// same transaction. Scheduling a later chapter does not erase this result.
func (s *Service) finishReleaseTx(ctx context.Context, tx pgx.Tx, id string, result json.RawMessage) error {
	if len(result) == 0 || !json.Valid(result) {
		return ErrInvalid
	}
	tag, err := tx.Exec(ctx, `UPDATE `+s.releaseRequestsTable()+` SET result=$3::jsonb,state='completed',error=NULL,
 completed_at=coalesce(completed_at,$4),updated_at=clock_timestamp()
 WHERE tenant_id=$1 AND id=$2 AND (current OR state='retiring') AND plan IS NOT NULL AND state<>'cancelled'
 AND (state<>'completed' OR result=$3::jsonb)`, s.o.Tenant, id, []byte(result), s.o.Now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errRetired
	}
	return nil
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
