package versions

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/media"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// MigrateReaderSnapshots recovers ready versions without a metadata snapshot
// through the ordinary durable publisher. It performs no object-store work and
// adds no recurring repair job. Completed snapshots are untouched on rerun.
func MigrateReaderSnapshots(ctx context.Context, pool *pgxpool.Pool, schema, tenant, riverSchema string, reg *media.Registry) error {
	var kinds []string
	for _, k := range reg.Kinds() {
		if k.Publish != nil {
			kinds = append(kinds, k.Name)
		}
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: riverSchema})
	if err != nil {
		return err
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		table := pgx.Identifier{"river_job"}.Sanitize()
		if riverSchema != "" {
			table = pgx.Identifier{riverSchema, "river_job"}.Sanitize()
		}
		rows, err := tx.Query(ctx, `SELECT id,state FROM `+table+` WHERE kind='contentkit_version_publish_recovery' AND state NOT IN('completed','cancelled','discarded') ORDER BY id FOR UPDATE`)
		if err != nil {
			return err
		}
		type oldJob struct {
			ID    int64
			State string
		}
		jobs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (oldJob, error) {
			var j oldJob
			err := r.Scan(&j.ID, &j.State)
			return j, err
		})
		if err != nil {
			return err
		}
		for _, job := range jobs {
			if job.State == "running" {
				return fmt.Errorf("versions: stop old publication recovery worker before migrating job %d", job.ID)
			}
			if _, err := client.JobCancelTx(ctx, tx, job.ID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if len(kinds) == 0 {
		return nil
	}
	items, versions, pubs := pgx.Identifier{schema, "content_items"}.Sanitize(), pgx.Identifier{schema, "content_versions"}.Sanitize(), pgx.Identifier{schema, "content_version_publications"}.Sanitize()
	after := ""
	for {
		var next string
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT v.content_id,v.id FROM `+versions+` v JOIN `+items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
WHERE v.tenant_id=$1 AND c.kind=ANY($2) AND v.id>$3 AND v.state<>'deleted' AND coalesce(v.pages,0)>0
AND NOT EXISTS(SELECT 1 FROM `+pubs+` p WHERE p.tenant_id=v.tenant_id AND p.version_id=v.id AND p.kind='pages' AND p.state='current')
ORDER BY v.id LIMIT 500`, tenant, kinds, after)
			if err != nil {
				return err
			}
			targets, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Target])
			if err != nil {
				return err
			}
			for _, t := range targets {
				if _, err := client.InsertTx(ctx, tx, publishArgs{Content: t.Content, Version: t.Version}, &river.InsertOpts{Queue: Queue, UniqueOpts: media.PendingOnce}); err != nil {
					return err
				}
				next = t.Version
			}
			return nil
		})
		if err != nil || next == "" {
			return err
		}
		after = next
	}
}
