package media

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
)

// ErrCommitPending means recovery must fence and settle an earlier write first.
var ErrCommitPending = errors.New("media: manifest commit recovery pending")

// ErrCommitIdentity means an operation ID was reused for different inputs.
var ErrCommitIdentity = errors.New("media: commit operation identity changed")

// TransactionalProcessQueue inserts and cancels processing in the journal's
// transaction. Implemented by workqueue.Queue in the same database.
type TransactionalProcessQueue interface {
	EnqueueTx(context.Context, pgx.Tx, ProcessJob) error
	CancelTx(context.Context, pgx.Tx, contentref.ContentRef) (int, error)
}

// PGJournal records unfinished S3 attempts and their database effects. The S3
// manifest remains authoritative; this table contains no current file list.
type PGJournal struct {
	pool    *pgxpool.Pool
	table   string
	limiter *PGLimiter
	queue   TransactionalProcessQueue
}

// NewPGJournal uses the migrated ContentKit schema and a processing queue in
// the same database. A nil queue supports manifest edits without queue effects.
// Quota policy is resolved by the uploader before preparing a transaction;
// recovery uses the recorded owner and charge, never a host policy callback.
func NewPGJournal(pool *pgxpool.Pool, schema string, queue TransactionalProcessQueue) (*PGJournal, error) {
	limiter, err := NewPGLimiter(pool, schema, PGLimits{})
	if err != nil {
		return nil, err
	}
	return &PGJournal{pool: pool, table: pgx.Identifier{schema, "content_media_commits"}.Sanitize(),
		limiter: limiter, queue: queue}, nil
}

type journalEffects struct {
	Settlement Settlement  `json:"settlement"`
	Cancel     bool        `json:"cancel,omitempty"`
	Process    *ProcessJob `json:"process,omitempty"`
}

type manifestCommit struct {
	Ref         contentref.ContentRef
	ID          uuid.UUID
	Folder      string
	Actor       string
	Fingerprint [sha256.Size]byte
	Lease       uuid.UUID
	Attempt     uuid.UUID
	ETag        string
	State       string
	Effects     journalEffects
	Charged     int64
}

func (c manifestCommit) terminal() bool { return c.State == "applied" || c.State == "absent" }

func (j *PGJournal) begin(ctx context.Context, c manifestCommit) (manifestCommit, error) {
	lease := uuid.New()
	err := pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO `+j.table+`
(tenant_id, operation_id, content_kind, content_id, folder_prefix, actor_id, fingerprint, lease_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (tenant_id, operation_id) DO NOTHING`,
			c.Ref.TenantID, c.ID, c.Ref.ContentKind, c.Ref.ContentID, c.Folder, c.Actor, c.Fingerprint[:], lease)
		if err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "content_media_commits_pending_folder" {
				return ErrCommitPending
			}
			return err
		}
		found, err := j.load(ctx, tx, c.Ref.TenantID, c.ID)
		if err != nil {
			return err
		}
		if found.Ref != c.Ref || found.Folder != c.Folder || found.Actor != c.Actor || found.Fingerprint != c.Fingerprint {
			return ErrCommitIdentity
		}
		if !found.terminal() && found.Lease != lease {
			return ErrCommitPending
		}
		c = found
		return nil
	})
	return c, err
}

func (j *PGJournal) load(ctx context.Context, tx pgx.Tx, tenant string, id uuid.UUID) (manifestCommit, error) {
	c := manifestCommit{ID: id, Ref: contentref.ContentRef{TenantID: tenant}}
	var fingerprint, effects []byte
	var attempt *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT content_kind, content_id, folder_prefix, actor_id, fingerprint,
lease_id, attempt_id, expected_etag, state, effects, charged_bytes FROM `+j.table+`
WHERE tenant_id = $1 AND operation_id = $2 FOR UPDATE`, tenant, id).Scan(
		&c.Ref.ContentKind, &c.Ref.ContentID, &c.Folder, &c.Actor, &fingerprint, &c.Lease,
		&attempt, &c.ETag, &c.State, &effects, &c.Charged)
	if err != nil {
		return c, err
	}
	copy(c.Fingerprint[:], fingerprint)
	if attempt != nil {
		c.Attempt = *attempt
	}
	if err := json.Unmarshal(effects, &c.Effects); err != nil {
		return c, fmt.Errorf("media: decode commit effects: %w", err)
	}
	return c, nil
}

// prepare records an attempt and its growth charge together, before S3 PUT.
// A frozen caller cannot refresh its expected ETag and start another attempt.
func (j *PGJournal) prepare(ctx context.Context, c *manifestCommit, effects journalEffects, quota int64) error {
	if effects.Settlement.Tenant != "" && effects.Settlement.Tenant != c.Ref.TenantID ||
		effects.Process != nil && effects.Process.Ref != c.Ref {
		return ErrCommitIdentity
	}
	if (effects.Cancel || effects.Process != nil) && j.queue == nil {
		return errors.New("media: commit processing requires a transactional queue")
	}
	body, err := json.Marshal(effects)
	if err != nil {
		return err
	}
	attempt := uuid.New()
	charged := max(effects.Settlement.Delta, 0)
	if effects.Settlement.Owner == "" {
		charged = 0
	}
	err = pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error {
		found, err := j.load(ctx, tx, c.Ref.TenantID, c.ID)
		if err != nil {
			return err
		}
		if found.Lease != c.Lease || found.State != "open" {
			return ErrCommitPending
		}
		if charged > 0 {
			s := effects.Settlement
			s.Keys, s.Delta = nil, charged
			if err := j.limiter.SettleTx(ctx, tx, s, quota); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE `+j.table+` SET state = 'prepared', attempt_id = $3,
expected_etag = $4, effects = $5, charged_bytes = $6, updated_at = now()
WHERE tenant_id = $1 AND operation_id = $2`, c.Ref.TenantID, c.ID, attempt, c.ETag, body, charged)
		return err
	})
	if err == nil {
		c.Attempt, c.Effects, c.Charged, c.State = attempt, effects, charged, "prepared"
	}
	return err
}

// freeze takes recovery ownership under the same row lock as prepare. It does
// not yet prove whether a PUT landed; only a subsequent S3 CAS can do that.
func (j *PGJournal) freeze(ctx context.Context, tenant, folder string, operation *uuid.UUID) (*manifestCommit, error) {
	var c *manifestCommit
	err := pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `SELECT operation_id FROM `+j.table+`
WHERE tenant_id = $1 AND folder_prefix = $2 AND state IN ('open', 'prepared', 'frozen')
AND ($3::uuid IS NULL OR operation_id = $3) FOR UPDATE`, tenant, folder, operation).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found, err := j.load(ctx, tx, tenant, id)
		if err != nil {
			return err
		}
		found.Lease, found.State = uuid.New(), "frozen"
		_, err = tx.Exec(ctx, `UPDATE `+j.table+` SET lease_id = $3, state = 'frozen', updated_at = now()
WHERE tenant_id = $1 AND operation_id = $2`, tenant, id, found.Lease)
		c = &found
		return err
	})
	return c, err
}

func (j *PGJournal) outcome(ctx context.Context, c manifestCommit) (string, error) {
	var state string
	err := j.pool.QueryRow(ctx, `SELECT state FROM `+j.table+`
WHERE tenant_id = $1 AND operation_id = $2`, c.Ref.TenantID, c.ID).Scan(&state)
	return state, err
}

// finish settles at most once. An absent outcome is legal only before prepare,
// or after recovery has fenced S3. Database effects and the outcome commit
// together, so a crash or failed enqueue leaves the original receipt pending.
func (j *PGJournal) finish(ctx context.Context, c manifestCommit, applied bool) error {
	return pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error {
		found, err := j.load(ctx, tx, c.Ref.TenantID, c.ID)
		if err != nil {
			return err
		}
		if found.terminal() {
			return nil
		}
		if found.Lease != c.Lease || !applied && found.State == "prepared" {
			return ErrCommitPending
		}
		if applied && (found.Effects.Cancel || found.Effects.Process != nil) && j.queue == nil {
			return errors.New("media: commit processing requires a transactional queue")
		}
		s := found.Effects.Settlement
		if applied {
			s.Delta -= found.Charged
		} else {
			s.Keys, s.Delta = nil, -found.Charged
		}
		s.Enforce = false
		if err := j.limiter.SettleTx(ctx, tx, s, 0); err != nil {
			return err
		}
		if applied && found.Effects.Cancel {
			if _, err := j.queue.CancelTx(ctx, tx, found.Ref); err != nil {
				return err
			}
		}
		if applied && found.Effects.Process != nil {
			if err := j.queue.EnqueueTx(ctx, tx, *found.Effects.Process); err != nil {
				return err
			}
		}
		state := "absent"
		if applied {
			state = "applied"
		}
		_, err = tx.Exec(ctx, `UPDATE `+j.table+` SET state = $3, updated_at = now()
WHERE tenant_id = $1 AND operation_id = $2`, found.Ref.TenantID, found.ID, state)
		return err
	})
}

// pending also discovers attempts that crashed before the first manifest PUT.
func (j *PGJournal) pending(ctx context.Context, tenant string, kinds []string, limit int) ([]manifestCommit, error) {
	rows, err := j.pool.Query(ctx, `SELECT operation_id, content_kind, content_id, folder_prefix FROM `+j.table+`
WHERE tenant_id = $1 AND content_kind = ANY($2) AND state IN ('open', 'prepared', 'frozen')
ORDER BY updated_at, operation_id LIMIT $3`, tenant, kinds, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []manifestCommit
	for rows.Next() {
		c := manifestCommit{Ref: contentref.ContentRef{TenantID: tenant}}
		if err := rows.Scan(&c.ID, &c.Ref.ContentKind, &c.Ref.ContentID, &c.Folder); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (c manifestCommit) receipt() *CommitReceipt {
	return &CommitReceipt{Operation: c.ID.String(), Attempt: c.Attempt.String()}
}

func (c manifestCommit) matches(r *CommitReceipt) bool {
	return r != nil && r.Operation == c.ID.String() && r.Attempt == c.Attempt.String()
}
