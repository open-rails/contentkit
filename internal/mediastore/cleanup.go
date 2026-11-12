package mediastore

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
	eventpg "github.com/open-rails/helpers/eventlog/postgres"
)

// scheduleCleanupTx merges one folder's cleanup obligations. Deletion keeps
// every exact operation. New work runs at the earliest due deadline; each
// receipt retains its own late-write deadline. Migration alone may extend it.
func (j *PGJournal) scheduleCleanupTx(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef, prefix string, due time.Time, operations []uuid.UUID, migrating bool) error {
	j = j.forTenant(ref.TenantID)
	if operations == nil {
		operations = []uuid.UUID{}
	}
	_, err := tx.Exec(ctx, `INSERT INTO `+j.cleanup+` AS c
(tenant_id, folder_prefix, content_kind, due_at, deletion_operations)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (tenant_id, folder_prefix) DO UPDATE SET
due_at = CASE WHEN $6 THEN greatest(c.due_at,EXCLUDED.due_at) ELSE least(c.due_at,EXCLUDED.due_at) END,
deletion_operations = CASE WHEN c.deletion_operations @> EXCLUDED.deletion_operations THEN c.deletion_operations ELSE ARRAY(SELECT DISTINCT op FROM unnest(c.deletion_operations || EXCLUDED.deletion_operations) AS op ORDER BY op) END,
revision = gen_random_uuid()`, ref.TenantID, prefix, ref.ContentKind, due, operations, migrating)
	return err
}

type cleanupRecord struct {
	tenant     string
	journal    *PGJournal
	due        time.Time
	prefix     string
	revision   uuid.UUID
	operations []uuid.UUID
}

const cleanupTurn = 1024

type cleanupResult struct {
	wait      time.Duration
	revision  uuid.UUID
	attempted []uuid.UUID
	settled   []uuid.UUID
	complete  bool
}

// Only errors.Join aggregates represent independent outcomes; multi-%w errors do not.
var joinedErrorType = fmt.Sprintf("%T", errors.Join(errors.New(""), errors.New("")))

func cleanupFailure(ctx context.Context, err error) error {
	if ctx.Err() == nil {
		return err
	}
	if fmt.Sprintf("%T", err) == joinedErrorType {
		var failures []error
		for _, part := range err.(interface{ Unwrap() []error }).Unwrap() {
			failures = append(failures, cleanupFailure(ctx, part))
		}
		return errors.Join(failures...)
	}
	if errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
}

func (j *PGJournal) checkpointCleanup(ctx context.Context, tenant, prefix string, result cleanupResult, cutoff time.Time) error {
	rotate := !result.complete && !cutoff.IsZero()
	if !rotate && len(result.attempted) == 0 && len(result.settled) == 0 {
		return nil
	}
	// Subtract and rotate from the current array, preserving concurrent work.
	_, err := j.pool.Exec(ctx, `UPDATE `+j.cleanup+` SET
deletion_operations=ARRAY(SELECT op FROM unnest(deletion_operations) WITH ORDINALITY u(op,i)
 WHERE op <> ALL(coalesce($3::uuid[],'{}')) ORDER BY op = ANY(coalesce($4::uuid[],'{}')),i),
due_at=CASE WHEN $5 AND due_at<=$6 THEN $6 + interval '1 microsecond' ELSE due_at END
WHERE tenant_id=$1 AND folder_prefix=$2`, tenant, prefix, result.settled, result.attempted, rotate, cutoff)
	if err != nil {
		return fmt.Errorf("media: checkpoint cleanup %s: %w", prefix, err)
	}
	return nil
}

// PurgeRetired runs one bounded cleanup turn per due folder, without bucket
// discovery. The count includes failed folders and never exceeds limit.
// Incomplete turns checkpoint progress and rotate behind untouched due work.
func (j *Jobs) PurgeRetired(ctx context.Context, limit int) (int, error) {
	ctx = context.WithValue(ctx, synchronousMutationOwnerKey{}, true)
	if limit <= 0 {
		return 0, errors.New("media: purge limit must be positive")
	}
	processed := 0
	cutoff := j.cfg.Now()
	var failures error
	var due []cleanupRecord
	for _, tenant := range j.cfg.Registry.Namespaces() {
		if err := ctx.Err(); err != nil {
			return processed, errors.Join(failures, err)
		}
		jr := j.cfg.Journal.forTenant(tenant)
		if jr != j.cfg.Journal {
			log, err := eventpg.New(jr.pool, eventpg.Config{Schema: jr.schema, Source: "contentkit", Scope: tenant})
			if err != nil {
				return processed, errors.Join(failures, err)
			}
			if _, err := log.Prune(ctx, limit); err != nil {
				if ctx.Err() != nil {
					return processed, errors.Join(failures, cleanupFailure(ctx, err), ctx.Err())
				}
				failures = errors.Join(failures, err)
			}
		}
		rows, err := jr.pool.Query(ctx, `SELECT folder_prefix,revision,deletion_operations,due_at FROM `+jr.cleanup+`
WHERE tenant_id=$1 AND content_kind=ANY($2) AND due_at<=$3 ORDER BY due_at,folder_prefix LIMIT $4`, tenant, j.cfg.Registry.cleanupKinds(tenant), cutoff, limit)
		if err != nil {
			return processed, errors.Join(failures, cleanupFailure(ctx, err), ctx.Err())
		}
		records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (cleanupRecord, error) {
			c := cleanupRecord{tenant: tenant, journal: jr}
			err := row.Scan(&c.prefix, &c.revision, &c.operations, &c.due)
			return c, err
		})
		if err != nil {
			return processed, errors.Join(failures, cleanupFailure(ctx, err), ctx.Err())
		}
		due = append(due, records...)
	}
	// Each namespace contributes at most limit candidates, but storage work
	// has one global budget. Fixed registry order cannot consume an account's turn.
	slices.SortFunc(due, func(a, b cleanupRecord) int {
		if order := a.due.Compare(b.due); order != 0 {
			return order
		}
		if order := cmp.Compare(a.tenant, b.tenant); order != 0 {
			return order
		}
		return cmp.Compare(a.prefix, b.prefix)
	})
	for _, c := range due[:min(limit, len(due))] {
		tenant, jr := c.tenant, c.journal
		if err := ctx.Err(); err != nil {
			return processed, errors.Join(failures, err)
		}
		processed++
		item, err := jr.resolveFolder(ctx, jr.pool, j.cfg.Registry, c.prefix)
		var result cleanupResult
		if err == nil && item.Ref().TenantID != tenant {
			err = ErrCommitIdentity
		}
		if err == nil {
			result, err = j.cleanupFolder(ctx, item, c.operations)
			if result.revision != uuid.Nil {
				c.revision = result.revision
			}
		} else {
			err = cleanupFailure(ctx, err)
		}
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("purge media %s: %w", c.prefix, err))
		}
		result.complete = result.complete && ctx.Err() == nil
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if err = jr.checkpointCleanup(bctx, tenant, c.prefix, result, cutoff); err != nil {
			cancel()
			return processed, errors.Join(failures, err, ctx.Err())
		}
		if !result.complete {
			cancel()
			continue
		}
		if result.wait > 0 {
			_, err = jr.pool.Exec(bctx, `UPDATE `+jr.cleanup+` SET due_at=$4,revision=gen_random_uuid()
WHERE tenant_id=$1 AND folder_prefix=$2 AND revision=$3`, tenant, c.prefix, c.revision, j.cfg.Now().Add(result.wait))
		} else {
			_, err = jr.pool.Exec(bctx, `DELETE FROM `+jr.cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2 AND revision=$3`, tenant, c.prefix, c.revision)
		}
		cancel()
		if err != nil {
			return processed, errors.Join(failures, fmt.Errorf("media: close cleanup %s: %w", c.prefix, err), ctx.Err())
		}
	}
	return processed, errors.Join(failures, ctx.Err())
}

func (j *Jobs) cleanupFolder(ctx context.Context, item Item, operations []uuid.UUID) (cleanupResult, error) {
	var result cleanupResult
	swept, current, revision, err := j.sweep(ctx, item.Prefix())
	if err != nil {
		return result, cleanupFailure(ctx, err)
	}
	result.wait, result.revision = swept.Wait, revision
	// Follow the locked array's order; append only extra candidate IDs.
	seen := make(map[uuid.UUID]bool, len(current)+len(operations))
	ordered := make([]uuid.UUID, 0, len(current)+len(operations))
	for _, id := range current {
		if !seen[id] {
			seen[id] = true
			ordered = append(ordered, id)
		}
	}
	for _, id := range operations {
		if !seen[id] {
			seen[id] = true
			ordered = append(ordered, id)
		}
	}
	operations = ordered[:min(cleanupTurn, len(ordered))]
	jr := j.cfg.Journal.forTenant(item.Ref().TenantID)
	// Plan chunk sizes and folder deletions; ordinary receipts are reloaded
	// after recovery under their chunk's folder lock.
	rows, err := jr.pool.Query(ctx, `SELECT operation_id,effects FROM `+jr.table+` WHERE tenant_id=$1 AND operation_id=ANY($2)`, item.Ref().TenantID, operations)
	if err != nil {
		return result, cleanupFailure(ctx, err)
	}
	effects := make(map[uuid.UUID][]byte, len(operations))
	for rows.Next() {
		var id uuid.UUID
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return result, cleanupFailure(ctx, err)
		}
		effects[id] = raw
	}
	if err := rows.Err(); err != nil {
		return result, cleanupFailure(ctx, err)
	}
	var failures error
	for start := 0; start < len(operations); {
		if ctx.Err() != nil {
			break
		}
		operation := operations[start]
		intent := manifestCommit{ID: operation}
		raw, found := effects[operation]
		err := json.Unmarshal(raw, &intent.Effects)
		if !found {
			err = pgx.ErrNoRows
		}
		if err != nil {
			result.attempted = append(result.attempted, operation)
			failures = errors.Join(failures, cleanupFailure(ctx, fmt.Errorf("media: deletion %s: %w", operation, err)))
			start++
			continue
		}
		if intent.Effects.FolderDelete != nil {
			result.attempted = append(result.attempted, operation)
			err = pgx.BeginFunc(ctx, jr.pool, func(tx pgx.Tx) error {
				var err error
				intent, err = jr.load(ctx, tx, item.Ref().TenantID, operation)
				return err
			})
			if err == nil {
				err = j.retireFolder(ctx, item, intent)
			}
			if err != nil {
				failures = errors.Join(failures, cleanupFailure(ctx, err))
			} else {
				result.wait = max(result.wait, intent.Effects.CleanupAt.Sub(j.cfg.Now()))
			}
			start++
			continue
		}
		end, keys := start, 0
		for end < len(operations) && end-start < 32 {
			var next journalEffects
			if err := json.Unmarshal(effects[operations[end]], &next); err != nil || next.FolderDelete != nil {
				break
			}
			n := len(next.Public) + len(next.Private)
			if end > start && keys+n > 256 {
				break
			}
			keys += n
			end++
		}
		chunk := operations[start:end]
		result.attempted = append(result.attempted, chunk...)
		settled, wait, err := j.replayDeletions(ctx, item, chunk)
		result.settled = append(result.settled, settled...)
		result.wait = max(result.wait, wait)
		failures = errors.Join(failures, err)
		start = end
	}
	result.complete = len(result.attempted) == len(ordered) && failures == nil && ctx.Err() == nil
	return result, failures
}

func (j *Jobs) replayDeletions(ctx context.Context, item Item, operations []uuid.UUID) ([]uuid.UUID, time.Duration, error) {
	unlock, err := j.cfg.Locker.Lock(ctx, item.ManifestKey())
	if err != nil {
		return nil, 0, cleanupFailure(ctx, err)
	}
	defer unlock()
	started := j.cfg.Now()
	if _, _, err := j.manifests.recoverLocked(ctx, item); err != nil {
		return nil, 0, cleanupFailure(ctx, err)
	}
	jr := j.cfg.Journal.forTenant(item.Ref().TenantID)
	rows, err := jr.pool.Query(ctx, `SELECT operation_id,content_kind,content_id,content_version_id,folder_prefix,state,effects
FROM `+jr.table+` WHERE tenant_id=$1 AND operation_id=ANY($2)`, item.Ref().TenantID, operations)
	if err != nil {
		return nil, 0, cleanupFailure(ctx, err)
	}
	var failures error
	type receipt struct {
		commit  manifestCommit
		effects []byte
	}
	receipts := make(map[uuid.UUID]receipt, len(operations))
	for rows.Next() {
		c := manifestCommit{Ref: item.Ref()}
		var raw []byte
		if err := rows.Scan(&c.ID, &c.Ref.ContentKind, &c.Ref.ContentID, &c.Ref.ContentVersionID, &c.Folder, &c.State, &raw); err != nil {
			rows.Close()
			return nil, 0, cleanupFailure(ctx, err)
		}
		receipts[c.ID] = receipt{commit: c, effects: raw}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, errors.Join(failures, cleanupFailure(ctx, err))
	}
	var candidates []manifestCommit
	for _, operation := range operations {
		r, ok := receipts[operation]
		if !ok {
			failures = errors.Join(failures, fmt.Errorf("media: deletion %s: %w", operation, pgx.ErrNoRows))
			continue
		}
		c := r.commit
		err := json.Unmarshal(r.effects, &c.Effects)
		if err == nil && (!c.Ref.Equal(item.Ref()) || c.Folder != item.Prefix() || c.State != "applied" || c.Effects.FolderDelete != nil) {
			err = ErrCommitPending
		}
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("media: deletion %s: %w", operation, err))
			continue
		}
		candidates = append(candidates, c)
	}
	current, _, err := j.manifests.get(ctx, item.ManifestKey())
	if errors.Is(err, ErrNotFound) {
		current = &Manifest{}
	} else if err != nil {
		return nil, 0, errors.Join(failures, cleanupFailure(ctx, err))
	}
	kept := j.keeps(item, current)
	seen := map[string]bool{}
	var keys []string
	var valid []manifestCommit
	for _, c := range candidates {
		c.Effects.Public = slices.DeleteFunc(c.Effects.Public, func(key string) bool {
			return kept[layout.AreaPublic+"/"+strings.TrimPrefix(key, item.PublicPrefix())]
		})
		referenced := false
		for _, key := range c.Effects.Private {
			if parsed, ok := layout.Parse(key); ok && kept[parsed.Area+"/"+parsed.Name] {
				referenced = true
				break
			}
		}
		if referenced {
			failures = errors.Join(failures, fmt.Errorf("media: deletion %s: retired allocation is still referenced", c.ID))
			continue
		}
		valid = append(valid, c)
		for _, key := range append(c.Effects.Public, c.Effects.Private...) {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	if err := j.deleteKeys(ctx, keys); err != nil {
		return nil, 0, errors.Join(failures, cleanupFailure(ctx, err))
	}
	var settled []uuid.UUID
	var wait time.Duration
	for _, c := range valid {
		if !c.Effects.CleanupAt.After(started) {
			settled = append(settled, c.ID)
		} else {
			wait = max(wait, c.Effects.CleanupAt.Sub(j.cfg.Now()), time.Microsecond)
		}
	}
	return settled, wait, failures
}

func (r *Registry) cleanupKinds(tenant string) []string {
	var out []string
	for _, kind := range r.all {
		if kind.ns == tenant {
			out = append(out, kind.Name)
		}
	}
	return out
}
