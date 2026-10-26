package contentkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media/versions"
)

var retiredPublicationKinds = []string{"contentkit_content_publish_due", "contentkit_catalog_publish_due"}

type publicationMigrationTarget struct {
	versions.Target
	Kind, State, ParentState string
	PublishAt, ParentAt      *time.Time
	PublishedAt              *time.Time
	Released                 bool
	ReleaseID                string
	Access, ReleasedAccess   access.Rule
	PolicyChanged            bool
	NextChapter              *time.Time
}

type publicationMigrationRequest struct {
	ID, State string
	Due       time.Time
	Frozen    bool
}

// preflightUnifiedReleases checks the old workers before any schema change.
// The full handoff repeats this check while excluding River claims/inserts.
func (e *Engine) preflightUnifiedReleases(ctx context.Context) error {
	jobs := pgx.Identifier{e.riverSchema, "river_job"}.Sanitize()
	var exists bool
	if err := e.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, jobs).Scan(&exists); err != nil || !exists {
		return err
	}
	rows, err := e.pool.Query(ctx, `SELECT id,kind,queue,state,args FROM `+jobs+`
 WHERE queue=ANY($1) AND kind=ANY($2) AND state='running' ORDER BY id`,
		[]string{catalog.Queue, versions.Queue, catalog.QueueFor(e.cfg.Tenant)}, retiredPublicationKinds)
	if err != nil {
		return err
	}
	old, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (publicationQueueRow, error) {
		var job publicationQueueRow
		err := row.Scan(&job.id, &job.kind, &job.queue, &job.state, &job.args)
		return job, err
	})
	if err != nil {
		return err
	}
	for _, job := range old {
		if job.queue == catalog.QueueFor(e.cfg.Tenant) {
			return errors.New("contentkit: stop legacy publication workers before the unified release migration")
		}
		owners, _, err := e.publicationJobOwners(ctx, e.pool, job)
		if err != nil {
			return err
		}
		if slices.Contains(owners, e.cfg.Tenant) {
			return errors.New("contentkit: stop legacy publication workers before the unified release migration")
		}
	}
	return nil
}

// migrateUnifiedReleases adopts completed state and admits exact outstanding
// work. It never executes publication or emits historical events. Old base
// queues must first pass migratePublicationQueues so each app owns its sweep.
func (e *Engine) migrateUnifiedReleases(ctx context.Context) error {
	client, err := river.NewClient(riverpgxv5.New(e.pool), &river.Config{Schema: e.riverSchema})
	if err != nil {
		return err
	}
	storage, err := e.publicationStorageKinds()
	if err != nil {
		return err
	}
	jobs := pgx.Identifier{e.riverSchema, "river_job"}.Sanitize()
	return pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `LOCK TABLE `+jobs+` IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		var running bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jobs+` WHERE queue=$1 AND kind=ANY($2) AND state='running')`,
			catalog.QueueFor(e.cfg.Tenant), retiredPublicationKinds).Scan(&running); err != nil {
			return err
		}
		if running {
			return errors.New("contentkit: stop legacy publication workers before the unified release migration")
		}
		old, err := e.legacyScopedPublicationJobs(ctx, tx, jobs)
		if err != nil {
			return err
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&now); err != nil {
			return err
		}
		// Scan canonical state even when a prior periodic row has been pruned.
		// Ordering locks every parent before its versions, like ordinary writes.
		for after := ""; ; {
			batch, next, err := e.publicationMigrationTargets(ctx, tx, after, now)
			if err != nil {
				return err
			}
			for _, target := range batch {
				if err := e.adoptPublicationTarget(ctx, tx, target, storage[target.Kind], now); err != nil {
					return fmt.Errorf("contentkit: migrate publication %s/%s: %w", target.Content, target.Version, err)
				}
			}
			if next == "" {
				break
			}
			after = next
		}
		for _, job := range old {
			if err := e.transferPublicationJob(ctx, tx, client, jobs, job); err != nil {
				return err
			}
		}
		// A superseded request can still own mandatory physical retirement.
		requests := pgx.Identifier{e.schema, "content_publication_requests"}.Sanitize()
		rows, err := tx.Query(ctx, `SELECT id::text,state,scheduled_at,plan IS NOT NULL FROM `+requests+`
 WHERE tenant_id=$1 AND (state='retiring' OR (current AND state IN ('pending','waiting','running','failed'))) ORDER BY id`, e.cfg.Tenant)
		if err != nil {
			return err
		}
		pending, err := pgx.CollectRows(rows, pgx.RowToStructByPos[publicationMigrationRequest])
		if err != nil {
			return err
		}
		for _, request := range pending {
			if err := e.ensurePublicationWake(ctx, tx, client, jobs, request); err != nil {
				return err
			}
		}
		return nil
	})
}

func (e *Engine) publicationStorageKinds() (map[string]bool, error) {
	out := map[string]bool{}
	if e.cfg.Media == nil {
		return out, nil
	}
	config, err := e.cfg.MediaKinds()
	if err != nil {
		return nil, err
	}
	for _, kind := range config.Kinds {
		if kind.Versions {
			out[kind.Name] = true
		}
	}
	return out, nil
}

func (e *Engine) legacyScopedPublicationJobs(ctx context.Context, tx pgx.Tx, jobs string) ([]publicationQueueRow, error) {
	rows, err := tx.Query(ctx, `SELECT id,kind,queue,state,args FROM `+jobs+`
 WHERE queue=$1 AND kind=ANY($2) AND state NOT IN ('completed','cancelled','discarded') ORDER BY id FOR UPDATE`, catalog.QueueFor(e.cfg.Tenant), retiredPublicationKinds)
	if err != nil {
		return nil, err
	}
	old, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (publicationQueueRow, error) {
		var job publicationQueueRow
		err := row.Scan(&job.id, &job.kind, &job.queue, &job.state, &job.args)
		return job, err
	})
	if err != nil {
		return nil, err
	}
	for _, job := range old {
		owners, periodic, err := e.publicationJobOwners(ctx, tx, job)
		if err != nil || !slices.Contains(owners, e.cfg.Tenant) || !periodic && len(owners) != 1 {
			return nil, fmt.Errorf("contentkit: publication job %d has ambiguous catalog ownership: %w", job.id, errors.Join(err, errors.New("target does not belong to its queue")))
		}
	}
	return old, nil
}

func (e *Engine) publicationMigrationTargets(ctx context.Context, tx pgx.Tx, after string, now time.Time) ([]publicationMigrationTarget, string, error) {
	items := pgx.Identifier{e.schema, "content_items"}.Sanitize()
	editions := pgx.Identifier{e.schema, "content_versions"}.Sanitize()
	segments := pgx.Identifier{e.schema, "content_segments"}.Sanitize()
	rows, err := tx.Query(ctx, `SELECT id FROM `+items+` WHERE tenant_id=$1 AND id>$2 ORDER BY id LIMIT 200 FOR UPDATE`, e.cfg.Tenant, after)
	if err != nil {
		return nil, "", err
	}
	parents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(parents) == 0 {
		return nil, "", err
	}
	rows, err = tx.Query(ctx, `SELECT v.content_id,v.id,c.kind,v.state,c.state,v.publish_at,c.publish_at,
 greatest(v.published_at,c.published_at),v.released,coalesce(v.release_id::text,''),v.access,coalesce(v.released_access,'{}'::jsonb),
 `+pgx.Identifier{e.schema, "content_frozen_access"}.Sanitize()+`(v.released_access,'infinity'::timestamptz) IS DISTINCT FROM `+pgx.Identifier{e.schema, "content_frozen_access"}.Sanitize()+`(v.access,$3),
 (SELECT min(s.release_at) FROM `+segments+` s WHERE s.tenant_id=v.tenant_id AND s.version_id=v.id AND s.released_at IS NULL AND s.release_at IS NOT NULL)
 FROM `+editions+` v JOIN `+items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 WHERE v.tenant_id=$1 AND c.id=ANY($2) ORDER BY c.id,v.id FOR UPDATE OF v`, e.cfg.Tenant, parents, now)
	if err != nil {
		return nil, "", err
	}
	targets, err := pgx.CollectRows(rows, pgx.RowToStructByPos[publicationMigrationTarget])
	return targets, parents[len(parents)-1], err
}

func (e *Engine) adoptPublicationTarget(ctx context.Context, tx pgx.Tx, target publicationMigrationTarget, storage bool, now time.Time) error {
	requests := pgx.Identifier{e.schema, "content_publication_requests"}.Sanitize()
	editions := pgx.Identifier{e.schema, "content_versions"}.Sanitize()
	var current publicationMigrationRequest
	err := tx.QueryRow(ctx, `SELECT id::text,state,scheduled_at,plan IS NOT NULL FROM `+requests+`
 WHERE tenant_id=$1 AND version_id=$2 AND current FOR UPDATE`, e.cfg.Tenant, target.Version).Scan(&current.ID, &current.State, &current.Due, &current.Frozen)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if !storage && target.Released && target.ReleaseID == "" {
		if target.PublishedAt == nil {
			return errors.New("released edition has no publication timestamp for baseline adoption")
		}
		baseline, err := versions.EncodeReleaseBaseline(target.Target, target.Kind, target.State, target.ParentState, target.Access, target.ReleasedAccess)
		if err != nil {
			return err
		}
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte("contentkit-release-baseline:"+e.schema+":"+e.cfg.Tenant+":"+target.Version)).String()
		if _, err := tx.Exec(ctx, `INSERT INTO `+requests+` (tenant_id,id,content_id,version_id,scheduled_at,state,plan,result,current,completed_at)
 VALUES($1,$2,$3,$4,$5,'completed',$6,$6,$7,$5)`, e.cfg.Tenant, id, target.Content, target.Version, *target.PublishedAt, baseline, current.ID == ""); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE `+editions+` SET release_id=$3 WHERE tenant_id=$1 AND id=$2 AND release_id IS NULL`, e.cfg.Tenant, target.Version, id); err != nil {
			return err
		}
		if current.ID == "" {
			current = publicationMigrationRequest{ID: id, State: "completed", Due: *target.PublishedAt, Frozen: true}
		}
	}
	if current.ID != "" && current.State != "completed" && current.State != "cancelled" {
		return nil // preserve accepted/frozen work and its exact due time
	}
	due := publicationMigrationDue(target, now)
	if due.IsZero() {
		return nil
	}
	if current.ID != "" {
		if _, err := tx.Exec(ctx, `UPDATE `+requests+` SET current=false WHERE tenant_id=$1 AND id=$2`, e.cfg.Tenant, current.ID); err != nil {
			return err
		}
	}
	request := publicationMigrationRequest{ID: uuid.NewString(), State: "pending", Due: due}
	if _, err := tx.Exec(ctx, `INSERT INTO `+requests+` (tenant_id,id,content_id,version_id,scheduled_at)
 VALUES($1,$2,$3,$4,$5)`, e.cfg.Tenant, request.ID, target.Content, target.Version, due); err != nil {
		return err
	}
	return nil
}

func publicationMigrationDue(target publicationMigrationTarget, now time.Time) time.Time {
	if target.State == "published" && target.ParentState == "published" {
		if !target.Released {
			due := now
			for _, at := range []*time.Time{target.PublishAt, target.ParentAt} {
				if at != nil && at.After(due) {
					due = *at
				}
			}
			return due
		}
		if target.PolicyChanged {
			return now
		}
		var due time.Time
		for _, release := range target.Access.Releases {
			if release.At.After(now) && (due.IsZero() || release.At.Before(due)) {
				due = release.At
			}
		}
		if target.NextChapter != nil && (due.IsZero() || target.NextChapter.Before(due)) {
			due = *target.NextChapter
		}
		return due
	}
	if target.Released && (target.State != "published" && target.State != "deleted" || target.ParentState != "published" && target.ParentState != "deleted") {
		return now
	}
	return time.Time{}
}

func (e *Engine) ensurePublicationWake(ctx context.Context, tx pgx.Tx, client *river.Client[pgx.Tx], jobs string, request publicationMigrationRequest) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jobs+`
 WHERE kind='contentkit_version_release' AND queue=$1 AND args->>'id'=$2 AND state IN ('available','pending','running','retryable','scheduled')
 AND (state='running' OR scheduled_at<=greatest($3,statement_timestamp())))`, versions.QueueFor(e.cfg.Tenant), request.ID, request.Due).Scan(&exists); err != nil || exists {
		return err
	}
	args, err := json.Marshal(struct {
		ID  string    `json:"id"`
		Due time.Time `json:"due"`
	}{request.ID, request.Due})
	if err != nil {
		return err
	}
	_, err = client.InsertTx(ctx, tx, migratedPublicationJob{kind: "contentkit_version_release", args: args}, &river.InsertOpts{Queue: versions.QueueFor(e.cfg.Tenant), ScheduledAt: request.Due})
	return err
}

func (e *Engine) transferPublicationJob(ctx context.Context, tx pgx.Tx, client *river.Client[pgx.Tx], jobs string, job publicationQueueRow) error {
	var args struct {
		Content string `json:"content"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(job.args, &args); err != nil {
		return err
	}
	if args.Content != "" && args.Version != "" {
		requests := pgx.Identifier{e.schema, "content_publication_requests"}.Sanitize()
		var request publicationMigrationRequest
		err := tx.QueryRow(ctx, `SELECT id::text,state,scheduled_at,plan IS NOT NULL FROM `+requests+`
 WHERE tenant_id=$1 AND content_id=$2 AND version_id=$3 AND current`, e.cfg.Tenant, args.Content, args.Version).Scan(&request.ID, &request.State, &request.Due, &request.Frozen)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			raw, err := json.Marshal(struct {
				ID  string    `json:"id"`
				Due time.Time `json:"due"`
			}{request.ID, request.Due})
			if err != nil {
				return err
			}
			// Keep the old row's timing, attempts and history. Its new worker
			// rechecks the request's authoritative due time before acting.
			_, err = tx.Exec(ctx, `UPDATE `+jobs+` SET kind='contentkit_version_release',queue=$2,args=$3,unique_key=NULL WHERE id=$1`, job.id, versions.QueueFor(e.cfg.Tenant), raw)
			return err
		}
	}
	// Empty sweeps and stale draft targets have no independent publication
	// authority. Exact accepted requests have already been admitted above.
	_, err := client.JobCancelTx(ctx, tx, job.id)
	return err
}

// validateUnifiedReleases is read-only: startup never adopts state or retires
// jobs. An explicit migration must finish each app's handoff first.
func (e *Engine) validateUnifiedReleases(ctx context.Context) error {
	jobs := pgx.Identifier{e.riverSchema, "river_job"}.Sanitize()
	var exists bool
	if err := e.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, jobs).Scan(&exists); err != nil || !exists {
		return err
	}
	if err := e.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jobs+`
 WHERE queue=$1 AND kind=ANY($2) AND state NOT IN ('completed','cancelled','discarded'))`, catalog.QueueFor(e.cfg.Tenant), retiredPublicationKinds).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return errors.New("contentkit: old publication jobs require the explicit unified release migration")
	}
	storage, err := e.publicationStorageKinds()
	if err != nil {
		return err
	}
	kinds := []string{}
	for name, stored := range storage {
		if stored {
			kinds = append(kinds, name)
		}
	}
	items := pgx.Identifier{e.schema, "content_items"}.Sanitize()
	editions := pgx.Identifier{e.schema, "content_versions"}.Sanitize()
	requests := pgx.Identifier{e.schema, "content_publication_requests"}.Sanitize()
	if err := e.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+editions+` v JOIN `+items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 WHERE v.tenant_id=$1 AND ((v.released AND v.release_id IS NULL AND NOT (c.kind=ANY($2::text[])))
 OR (c.state='published' AND v.state='published' AND NOT v.released
 AND NOT EXISTS(SELECT 1 FROM `+requests+` r WHERE r.tenant_id=v.tenant_id AND r.version_id=v.id AND r.current AND r.state NOT IN ('completed','cancelled')))))`, e.cfg.Tenant, kinds).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return errors.New("contentkit: accepted publication state requires the explicit unified release migration")
	}
	return nil
}
