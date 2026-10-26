package contentkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

var legacyPublicationKinds = []string{
	"contentkit_content_publish_due", "contentkit_catalog_publish_due",
	"contentkit_content_purge_deleted", "contentkit_catalog_purge_deleted",
	"contentkit_version_release",
}

type migratedPublicationJob struct {
	kind string
	args json.RawMessage
}

func (j migratedPublicationJob) Kind() string                 { return j.kind }
func (j migratedPublicationJob) MarshalJSON() ([]byte, error) { return j.args, nil }

type publicationQueueRow struct {
	id          int64
	kind, queue string
	state       rivertype.JobState
	args        json.RawMessage
}

// migratePublicationQueues transfers exact legacy obligations after old
// workers stop. Targeted rows keep their IDs, attempts and scheduling history.
// Unscoped periodic obligations are expanded to every catalog in this schema
// before their old row is cancelled. Foreign queues and terminal rows stay.
func (e *Engine) migratePublicationQueues(ctx context.Context) error {
	client, err := river.NewClient(riverpgxv5.New(e.pool), &river.Config{Schema: e.riverSchema})
	if err != nil {
		return err
	}
	table := pgx.Identifier{e.riverSchema, "river_job"}.Sanitize()
	return pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id,kind,queue,state,args FROM `+table+`
 WHERE queue=ANY($1) AND kind=ANY($2) AND state NOT IN ('completed','cancelled','discarded') ORDER BY id FOR UPDATE`,
			[]string{catalog.Queue, versions.Queue}, legacyPublicationKinds)
		if err != nil {
			return err
		}
		jobs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (publicationQueueRow, error) {
			var job publicationQueueRow
			err := row.Scan(&job.id, &job.kind, &job.queue, &job.state, &job.args)
			return job, err
		})
		if err != nil {
			return err
		}
		for _, job := range jobs {
			owners, periodic, err := e.publicationJobOwners(ctx, tx, job)
			if err != nil {
				return fmt.Errorf("contentkit: publication queue job %d: %w", job.id, err)
			}
			if len(owners) == 0 {
				continue // no authoritative owner in this catalog schema
			}
			if job.state == rivertype.JobStateRunning {
				return fmt.Errorf("contentkit: stop legacy publication workers before migrating running job %d", job.id)
			}
			queueFor := catalog.QueueFor
			if strings.HasPrefix(job.kind, "contentkit_version_") {
				queueFor = versions.QueueFor
			}
			if !periodic {
				if len(owners) != 1 {
					return fmt.Errorf("contentkit: publication queue job %d has ambiguous catalog ownership", job.id)
				}
				if _, err := tx.Exec(ctx, `UPDATE `+table+` SET queue=$2 WHERE id=$1`, job.id, queueFor(owners[0])); err != nil {
					return err
				}
				continue
			}
			kind := job.kind
			if kind == "contentkit_catalog_publish_due" {
				kind = "contentkit_content_publish_due"
			} else if kind == "contentkit_catalog_purge_deleted" {
				kind = "contentkit_content_purge_deleted"
			}
			for _, owner := range owners {
				unique := media.PendingOnce
				unique.ByQueue = true
				if _, err := client.InsertTx(ctx, tx, migratedPublicationJob{kind: kind, args: json.RawMessage(`{}`)},
					&river.InsertOpts{Queue: queueFor(owner), UniqueOpts: unique}); err != nil {
					return err
				}
			}
			if _, err := client.JobCancelTx(ctx, tx, job.id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (e *Engine) publicationJobOwners(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, job publicationQueueRow) ([]string, bool, error) {
	var args struct {
		Content string   `json:"content"`
		Version string   `json:"version"`
		From    string   `json:"from"`
		To      []string `json:"to"`
		ID      string   `json:"id"`
	}
	if err := json.Unmarshal(job.args, &args); err != nil {
		return nil, false, err
	}
	items := pgx.Identifier{e.schema, "content_items"}.Sanitize()
	editions := pgx.Identifier{e.schema, "content_versions"}.Sanitize()
	periodic := args.Content == "" && (job.kind == "contentkit_content_publish_due" || job.kind == "contentkit_catalog_publish_due" ||
		job.kind == "contentkit_content_purge_deleted" || job.kind == "contentkit_catalog_purge_deleted")
	var query string
	var values []any
	switch {
	case periodic:
		var extra map[string]json.RawMessage
		if err := json.Unmarshal(job.args, &extra); err != nil || len(extra) != 0 {
			return nil, false, errors.New("unscoped periodic publication job has unexpected arguments")
		}
		query = `SELECT tenant_id FROM ` + items + ` UNION SELECT catalog_key FROM ` + pgx.Identifier{e.schema, "content_app_bindings"}.Sanitize() + ` UNION SELECT $1::text ORDER BY 1`
		values = []any{e.cfg.Tenant}
	case job.kind == "contentkit_version_release":
		query = `SELECT tenant_id FROM ` + pgx.Identifier{e.schema, "content_publication_requests"}.Sanitize() + ` WHERE id::text=$1`
		values = []any{args.ID}
	case args.Content != "":
		query = `SELECT c.tenant_id FROM ` + items + ` c WHERE c.id=$1
 AND ($2='' OR EXISTS(SELECT 1 FROM ` + editions + ` v WHERE v.tenant_id=c.tenant_id AND v.content_id=c.id AND v.id=$2))
 AND ($3='' OR EXISTS(SELECT 1 FROM ` + editions + ` v WHERE v.tenant_id=c.tenant_id AND v.content_id=c.id AND v.id=$3))
 AND NOT EXISTS(SELECT 1 FROM unnest($4::text[]) wanted(id) WHERE NOT EXISTS
 (SELECT 1 FROM ` + editions + ` v WHERE v.tenant_id=c.tenant_id AND v.content_id=c.id AND v.id=wanted.id))`
		values = []any{args.Content, args.Version, args.From, args.To}
	default:
		return nil, false, errors.New("targeted publication job has no resolvable content identity")
	}
	rows, err := q.Query(ctx, query, values...)
	if err != nil {
		return nil, periodic, err
	}
	owners, err := pgx.CollectRows(rows, pgx.RowTo[string])
	slices.Sort(owners)
	return slices.Compact(owners), periodic, err
}

// validatePublicationQueues is a read-only startup guard. Transfer remains an
// explicit migration action; starting a worker never rewrites queue ownership.
func (e *Engine) validatePublicationQueues(ctx context.Context) error {
	table := pgx.Identifier{e.riverSchema, "river_job"}.Sanitize()
	rows, err := e.pool.Query(ctx, `SELECT id,kind,queue,state,args FROM `+table+`
 WHERE queue=ANY($1) AND kind=ANY($2) AND state NOT IN ('completed','cancelled','discarded') ORDER BY id`,
		[]string{catalog.Queue, versions.Queue}, legacyPublicationKinds)
	if err != nil {
		return err
	}
	jobs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (publicationQueueRow, error) {
		var job publicationQueueRow
		err := row.Scan(&job.id, &job.kind, &job.queue, &job.state, &job.args)
		return job, err
	})
	if err != nil {
		return err
	}
	for _, job := range jobs {
		owners, _, err := e.publicationJobOwners(ctx, e.pool, job)
		if err != nil {
			return err
		}
		if slices.Contains(owners, e.cfg.Tenant) {
			return fmt.Errorf("contentkit: publication job %d needs explicit scoped-queue migration", job.id)
		}
	}
	return nil
}
