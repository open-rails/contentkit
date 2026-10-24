package media

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

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
	schema       string
	pool         *pgxpool.Pool
	table        string
	allocations  string
	publications string
	cleanup      string
	limiter      *PGLimiter
	queue        TransactionalProcessQueue
	routes       map[string]*PGJournal
}

// NewPGJournal uses the migrated ContentKit schema and a processing queue in
// the same database. A nil queue supports reads and owner-driven erasure only.
// Quota policy is resolved by the uploader before preparing a transaction;
// recovery uses the recorded owner and charge, never a host policy callback.
func NewPGJournal(pool *pgxpool.Pool, schema string, queue TransactionalProcessQueue) (*PGJournal, error) {
	limiter, err := NewPGLimiter(pool, schema, PGLimits{})
	if err != nil {
		return nil, err
	}
	return &PGJournal{schema: schema, pool: pool, table: pgx.Identifier{schema, "content_media_commits"}.Sanitize(),
		allocations:  pgx.Identifier{schema, "content_media_allocations"}.Sanitize(),
		publications: pgx.Identifier{schema, "content_media_publications"}.Sanitize(),
		cleanup:      pgx.Identifier{schema, "content_media_cleanup"}.Sanitize(),
		limiter:      limiter, queue: queue}, nil
}

type folderDeletion struct {
	Owner       string `json:"owner,omitempty"`
	Incarnation string `json:"incarnation"`
	Completed   bool   `json:"completed,omitempty"`
}

type journalEffects struct {
	FolderDelete       *folderDeletion           `json:"folder_delete,omitempty"`
	Manifest           []byte                    `json:"manifest,omitempty"` // exact admitted conditional PUT, owned by its ingest/processor job
	Ingest             bool                      `json:"ingest,omitempty"`   // processing was admitted before the S3 write
	VersionStoragePlan *versionStoragePlan       `json:"version_storage_plan,omitempty"`
	VersionStorage     *versionStorageRetirement `json:"version_storage,omitempty"`
	LegacyPublication  *legacyPublication        `json:"legacy_publication,omitempty"`
	CleanupAt          time.Time                 `json:"cleanup_at,omitempty"`
	CleanupDelete      bool                      `json:"cleanup_delete,omitempty"` // repeat this operation's exact retired targets
	Deletion           bool                      `json:"deletion,omitempty"`       // explicit purge may fence an unreadable root without decoding it
	Erasure            bool                      `json:"erasure,omitempty"`        // permanent lifecycle; a version's catalog purge owns retries
	Cleanup            bool                      `json:"cleanup,omitempty"`        // allocation retirement, no manifest PUT
	Allocate           []string                  `json:"allocate,omitempty"`       // physical names owned before a producer sends bytes
	Adopt              []string                  `json:"adopt,omitempty"`          // a legacy item's existing objects, owned by the upgrade's incarnation
	Incarnation        string                    `json:"incarnation,omitempty"`    // allocation lifetime, read from S3 under this lease
	Settlement         Settlement                `json:"settlement"`
	Cancel             bool                      `json:"cancel,omitempty"`
	Process            *ProcessJob               `json:"process,omitempty"`
	Public             []string                  `json:"public,omitempty"`  // exact retired public names, selected before PUT
	Mirror             []Mirror                  `json:"mirror,omitempty"`  // Stable presets' new current files, copied to their logical names
	Private            []string                  `json:"private,omitempty"` // exact takedown targets, selected before PUT
}

// prepareCleanup retires exact allocations while the operation still owns
// the folder. A cleaner that lost its lease cannot delete from an old snapshot.
// The projection, without what this cleanup retires, commits with them.
// lockAdmission orders irreversible owner claims against new mutation/allocation
// admission without holding a catalog row lock across storage I/O.
func (j *PGJournal) lockAdmission(ctx context.Context, tx pgx.Tx, folder string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, j.schema+"/media-admission/"+folder)
	return err
}

func (j *PGJournal) writableTx(ctx context.Context, tx pgx.Tx, tenant, folder, incarnation string) error {
	var retired bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+j.table+` WHERE tenant_id=$1 AND folder_prefix=$2
AND effects->'folder_delete'->>'incarnation'=$3)`, tenant, folder, incarnation).Scan(&retired)
	if err == nil && retired {
		return ErrNotFound
	}
	return err
}

func (j *PGJournal) prepareCleanup(ctx context.Context, c *manifestCommit, effects journalEffects, proj *projection) error {
	j = j.forTenant(c.Ref.TenantID)
	effects.Cleanup = true
	body, err := json.Marshal(effects)
	if err != nil {
		return err
	}
	attempt := uuid.New()
	err = pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error {
		if len(effects.Allocate) > 0 {
			if err := j.lockAdmission(ctx, tx, c.Folder); err != nil {
				return err
			}
			if err := j.writableTx(ctx, tx, c.Ref.TenantID, c.Folder, effects.Incarnation); err != nil {
				return err
			}
		}
		found, err := j.load(ctx, tx, c.Ref.TenantID, c.ID)
		if err != nil {
			return err
		}
		if found.Lease != c.Lease || found.State != "open" {
			return ErrCommitPending
		}
		for _, key := range effects.Allocate {
			if _, err := tx.Exec(ctx, `INSERT INTO `+j.allocations+`
(tenant_id, folder_prefix, object_key, incarnation) VALUES ($1, $2, $3, $4)`,
				c.Ref.TenantID, c.Folder, key, effects.Incarnation); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE `+j.allocations+` SET retired_at = now()
WHERE tenant_id = $1 AND folder_prefix = $2 AND object_key = ANY($3::text[]) AND retired_at IS NULL`,
			c.Ref.TenantID, c.Folder, effects.Private); err != nil {
			return err
		}
		if err := j.projectTx(ctx, tx, proj); err != nil {
			return err
		}
		due := effects.CleanupAt
		if len(effects.Private)+len(effects.Public) > 0 {
			due = time.Now()
		}
		if !due.IsZero() {
			if err := j.scheduleCleanupTx(ctx, tx, c.Ref, c.Folder, due, nil, false); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE `+j.table+` SET state = 'prepared', attempt_id = $3, effects = $4, updated_at = now()
WHERE tenant_id = $1 AND operation_id = $2`, c.Ref.TenantID, c.ID, attempt, body)
		return err
	})
	if err == nil {
		c.Attempt, c.Effects, c.State = attempt, effects, "prepared"
	}
	return err
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

// claimErasureTx is a completed intent, not an unfinished manifest write. It
// makes restoration refuse the version before the first S3 request is issued.
func (j *PGJournal) claimErasureTx(ctx context.Context, tx pgx.Tx, item Item) error {
	j = j.forTenant(item.Ref().TenantID)
	operation := uuid.NewSHA1(uuid.NameSpaceURL, []byte("erasure-claim:"+item.Prefix()))
	fingerprint := sha256.Sum256([]byte("permanent-version-erasure"))
	_, err := tx.Exec(ctx, `INSERT INTO `+j.table+`
(tenant_id,operation_id,content_kind,content_id,content_version_id,folder_prefix,actor_id,fingerprint,lease_id,state,effects)
VALUES($1,$2,$3,$4,$5,$6,'',$7,$8,'applied','{"erasure":true}')
ON CONFLICT(tenant_id,operation_id) DO NOTHING`, item.Ref().TenantID, operation, item.Ref().ContentKind,
		item.Ref().ContentID, item.Ref().ContentVersionID, item.Prefix(), fingerprint[:], uuid.New())
	if err != nil {
		return err
	}
	c, err := j.load(ctx, tx, item.Ref().TenantID, operation)
	if err != nil {
		return err
	}
	if !c.Ref.Equal(item.Ref()) || c.Folder != item.Prefix() || c.State != "applied" || !c.Effects.Erasure || c.Fingerprint != fingerprint {
		return ErrCommitIdentity
	}
	return nil
}

func (c manifestCommit) terminal() bool { return c.State == "applied" || c.State == "absent" }

func (j *PGJournal) begin(ctx context.Context, c manifestCommit) (manifestCommit, error) {
	j = j.forTenant(c.Ref.TenantID)
	lease := uuid.New()
	effects, err := json.Marshal(c.Effects)
	if err != nil {
		return c, err
	}
	err = pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error {
		if c.Ref.Version() != "" {
			if err := j.checkFolderOwner(ctx, tx, Item{ref: c.Ref, prefix: c.Folder}, false); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO `+j.table+`
(tenant_id, operation_id, content_kind, content_id, content_version_id, folder_prefix, actor_id, fingerprint, lease_id, effects)
VALUES ($1, $2, $3, $4, $10, $5, $6, $7, $8, $9) ON CONFLICT (tenant_id, operation_id) DO NOTHING`,
			c.Ref.TenantID, c.ID, c.Ref.ContentKind, c.Ref.ContentID, c.Folder, c.Actor, c.Fingerprint[:], lease, effects, c.Ref.ContentVersionID)
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
		if !found.Ref.Equal(c.Ref) || found.Folder != c.Folder || found.Actor != c.Actor || found.Fingerprint != c.Fingerprint {
			return ErrCommitIdentity
		}
		if !found.terminal() && found.Lease != lease {
			return ErrCommitPending
		}
		if found.State == "absent" {
			// Recovery fenced the old attempt and refunded its claim. A new
			// authorized request may retry these same inputs with a new lease.
			_, err = tx.Exec(ctx, `UPDATE `+j.table+` SET state = 'open', lease_id = $3,
attempt_id = NULL, expected_etag = '', effects = $4, charged_bytes = 0, updated_at = now()
WHERE tenant_id = $1 AND operation_id = $2`, c.Ref.TenantID, c.ID, lease, effects)
			if err != nil {
				return err
			}
			found.Lease, found.State, found.Attempt = lease, "open", uuid.Nil
			found.ETag, found.Effects, found.Charged = "", c.Effects, 0
		}
		c = found
		return nil
	})
	return c, err
}

// existing verifies identity before a retry can copy bytes or reserve quota.
func (j *PGJournal) existing(ctx context.Context, c manifestCommit) (*manifestCommit, error) {
	j = j.forTenant(c.Ref.TenantID)
	var out *manifestCommit
	err := pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error {
		found, err := j.load(ctx, tx, c.Ref.TenantID, c.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !found.Ref.Equal(c.Ref) || found.Folder != c.Folder || found.Actor != c.Actor || found.Fingerprint != c.Fingerprint {
			return ErrCommitIdentity
		}
		out = &found
		return nil
	})
	return out, err
}

func (j *PGJournal) load(ctx context.Context, tx pgx.Tx, tenant string, id uuid.UUID) (manifestCommit, error) {
	j = j.forTenant(tenant)
	c := manifestCommit{ID: id, Ref: contentref.ContentRef{TenantID: tenant}}
	var fingerprint, effects []byte
	var attempt *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT content_kind, content_id, content_version_id, folder_prefix, actor_id, fingerprint,
lease_id, attempt_id, expected_etag, state, effects, charged_bytes FROM `+j.table+`
WHERE tenant_id = $1 AND operation_id = $2 FOR UPDATE`, tenant, id).Scan(
		&c.Ref.ContentKind, &c.Ref.ContentID, &c.Ref.ContentVersionID, &c.Folder, &c.Actor, &fingerprint, &c.Lease,
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
func (j *PGJournal) prepare(ctx context.Context, c *manifestCommit, effects journalEffects, quota int64, manifest []byte, incarnation string, lifecycle bool) error {
	j = j.forTenant(c.Ref.TenantID)
	if effects.Settlement.Tenant != "" && effects.Settlement.Tenant != c.Ref.TenantID ||
		effects.Process != nil && !effects.Process.Ref.Equal(c.Ref) {
		return ErrCommitIdentity
	}
	if (effects.Cancel || effects.Process != nil) && j.queue == nil {
		return errors.New("media: commit processing requires a transactional queue")
	}
	attempt := c.Attempt
	effects.Manifest = manifest
	// Existing jobs own their writes. A request admits ingest in the same
	// transaction as quota and the exact mutation, before any storage write.
	_, ownerErr := river.ClientFromContextSafely[pgx.Tx](ctx)
	purging, _ := ctx.Value(purgeContextKey{}).(bool)
	effects.Ingest = !lifecycle && !purging && ownerErr != nil
	if effects.Ingest && j.queue == nil {
		return errors.New("media: manifest mutation needs a durable ingest queue")
	}
	body, err := json.Marshal(effects)
	if err != nil {
		return err
	}
	charged := max(effects.Settlement.Delta, 0)
	if effects.Settlement.Owner == "" {
		charged = 0
	}
	err = pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error {
		if !lifecycle {
			if err := j.lockAdmission(ctx, tx, c.Folder); err != nil {
				return err
			}
			if err := j.writableTx(ctx, tx, c.Ref.TenantID, c.Folder, incarnation); err != nil {
				return err
			}
		}
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
		if err := j.adoptTx(ctx, tx, c, effects); err != nil {
			return err
		}
		if effects.Ingest {
			if effects.Cancel {
				if _, err := j.queue.CancelTx(ctx, tx, c.Ref); err != nil {
					return err
				}
			}
			job := ProcessJob{Ref: c.Ref}
			if effects.Process != nil {
				job = *effects.Process
			}
			job.Commit, job.Place = c.ID.String(), true
			if err := j.queue.EnqueueTx(ctx, tx, job); err != nil {
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
	j = j.forTenant(tenant)
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
	j = j.forTenant(c.Ref.TenantID)
	var state string
	err := j.pool.QueryRow(ctx, `SELECT state FROM `+j.table+`
WHERE tenant_id = $1 AND operation_id = $2`, c.Ref.TenantID, c.ID).Scan(&state)
	return state, err
}

// checkReceipt refuses a writer pointed at another journal schema. A known
// completed receipt can remain in S3 until the next write; an unknown receipt
// must never be erased just because this schema has no pending row.
func (j *PGJournal) checkReceipt(ctx context.Context, item Item, r *CommitReceipt) error {
	j = j.forTenant(item.Ref().TenantID)
	if r == nil {
		return nil
	}
	operation, err := uuid.Parse(r.Operation)
	if err != nil {
		return ErrCommitIdentity
	}
	attempt, err := uuid.Parse(r.Attempt)
	if err != nil {
		return ErrCommitIdentity
	}
	var known bool
	err = j.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+j.table+`
WHERE tenant_id = $1 AND operation_id = $2 AND attempt_id = $3
AND content_kind = $4 AND content_id = $5 AND content_version_id IS NOT DISTINCT FROM $7::text AND folder_prefix = $6 AND state = 'applied')`,
		item.Ref().TenantID, operation, attempt, item.Ref().ContentKind, item.Ref().ContentID, item.Prefix(), item.Ref().ContentVersionID).Scan(&known)
	if err != nil {
		return err
	}
	if !known {
		return ErrCommitPending
	}
	return nil
}

// adoptTx records a legacy item's objects under the upgrade's incarnation
// before its manifest PUT. A retry after an absent attempt moves them to its
// own incarnation; a retired object is never adopted again. Keys repeat when
// files share a blob; each is adopted once.
func (j *PGJournal) adoptTx(ctx context.Context, tx pgx.Tx, c *manifestCommit, effects journalEffects) error {
	j = j.forTenant(c.Ref.TenantID)
	if len(effects.Adopt) == 0 {
		return nil
	}
	var all bool
	err := tx.QueryRow(ctx, `WITH k AS (SELECT DISTINCT key FROM unnest($3::text[]) AS key),
a AS (INSERT INTO `+j.allocations+` AS a (tenant_id, folder_prefix, object_key, incarnation)
SELECT $1, $2, key, $4 FROM k
ON CONFLICT (tenant_id, object_key) DO UPDATE SET incarnation = EXCLUDED.incarnation
WHERE a.folder_prefix = EXCLUDED.folder_prefix AND a.retired_at IS NULL RETURNING 1)
SELECT (SELECT count(*) FROM a) = (SELECT count(*) FROM k)`, c.Ref.TenantID, c.Folder, effects.Adopt, effects.Incarnation).Scan(&all)
	if err != nil {
		return err
	}
	if !all {
		return ErrAllocationRetired
	}
	return nil
}

// finish settles at most once. An absent outcome is legal only before prepare,
// or after recovery has fenced S3. Database effects and the outcome commit
// together, so a crash or failed enqueue leaves the original receipt pending.
// proj, the authoritative manifest's publications, is written with them.
func (j *PGJournal) finish(ctx context.Context, c manifestCommit, applied bool, proj *projection) error {
	j = j.forTenant(c.Ref.TenantID)
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
		if applied && found.Effects.Cancel && !found.Effects.Ingest {
			if _, err := j.queue.CancelTx(ctx, tx, found.Ref); err != nil {
				return err
			}
		}
		if applied && found.Effects.Process != nil && !found.Effects.Ingest {
			if err := j.queue.EnqueueTx(ctx, tx, *found.Effects.Process); err != nil {
				return err
			}
		}
		if err := j.projectTx(ctx, tx, proj); err != nil {
			return err
		}
		state := "absent"
		if applied {
			state = "applied"
			if !found.Effects.CleanupAt.IsZero() {
				var operations []uuid.UUID
				if found.Effects.CleanupDelete {
					operations = []uuid.UUID{found.ID}
				}
				due := found.Effects.CleanupAt
				if len(found.Effects.Public)+len(found.Effects.Private) > 0 {
					due = time.Now()
				}
				if err := j.scheduleCleanupTx(ctx, tx, found.Ref, found.Folder, due, operations, false); err != nil {
					return err
				}
			}
		}
		_, err = tx.Exec(ctx, `UPDATE `+j.table+` SET state = $3, updated_at = now()
WHERE tenant_id = $1 AND operation_id = $2`, found.Ref.TenantID, found.ID, state)
		return err
	})
}

func (c manifestCommit) receipt() *CommitReceipt {
	return &CommitReceipt{Operation: c.ID.String(), Attempt: c.Attempt.String()}
}

func (c manifestCommit) matches(r *CommitReceipt) bool {
	return r != nil && r.Operation == c.ID.String() && r.Attempt == c.Attempt.String()
}

// RouteJournal keeps tenant account receipts and allocation state in the
// account schema while app media continues to use its catalog schema. Both
// journals must use the same database so a commit keeps all effects atomic.
func RouteJournal(app, account *PGJournal, partition string) (*PGJournal, error) {
	if app == nil || account == nil || app.pool != account.pool || partition == "" {
		return nil, errors.New("media: journal route needs one database and account partition")
	}
	copy := *app
	copy.routes = map[string]*PGJournal{partition: account}
	return &copy, nil
}
func (j *PGJournal) forTenant(tenant string) *PGJournal {
	if q := j.routes[tenant]; q != nil {
		return q
	}
	return j
}

func (j *PGJournal) all() []*PGJournal {
	out := []*PGJournal{j}
	for _, v := range j.routes {
		out = append(out, v)
	}
	return out
}
