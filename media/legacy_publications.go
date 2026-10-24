package media

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// legacyPublication is a migration receipt, never a registered media kind.
// The source proves ownership but is never one of its deletion targets.
type legacyPublication struct {
	Kind           string     `json:"kind"`
	Content        string     `json:"content"`
	Version        string     `json:"version"`
	Source         string     `json:"source"`
	PreviousDue    *time.Time `json:"previous_due"`
	Initialized    bool       `json:"initialized"`
	Final          bool       `json:"final"`
	PriorOperation string     `json:"prior_operation,omitempty"`
	DeletionJob    int64      `json:"deletion_job,omitempty"`
	Incarnation    string     `json:"incarnation,omitempty"`
}

func (r *Registry) cleanupKinds(tenant string) []string {
	var out []string
	for _, kind := range r.all {
		if kind.ns != tenant {
			continue
		}
		out = append(out, kind.Name)
		if kind.Versions {
			out = append(out, kind.Name+PublishedSuffix)
		}
	}
	return out
}

// cleanupItem permits historical destinations only in the cleanup path. They
// remain absent from Kind, Ref, read routes, uploads and worker registries.
func (r *Registry) cleanupItem(prefix string) (Item, bool, error) {
	item, err := r.folderItem(prefix)
	if err == nil {
		return item, false, nil
	}
	ref, parseErr := parseFolder(prefix)
	if parseErr != nil || ref.Version() != "" || !strings.HasSuffix(ref.ContentKind, PublishedSuffix) {
		return Item{}, false, err
	}
	parent, e := r.Kind(strings.TrimSuffix(ref.ContentKind, PublishedSuffix))
	if e != nil || !parent.Versions {
		return Item{}, false, err
	}
	want := parent.scope.Prefix(ref.ContentKind, ref.ContentID, "")
	if parent.scope.IsZero() {
		want = layout.Prefix(parent.ns, ref.ContentKind, ref.ContentID, "")
	}
	if ref.TenantID != parent.ns || prefix != want {
		return Item{}, false, errors.New("media: legacy cleanup prefix leaves its owner scope")
	}
	kind := &Kind{Name: ref.ContentKind, kindState: kindState{ns: parent.ns, scope: parent.scope}}
	return Item{ref: ref, kind: kind, prefix: prefix}, true, nil
}

func (j *Jobs) cleanLegacyPublication(ctx context.Context, item Item, operations []uuid.UUID) error {
	if len(operations) == 0 {
		return errors.New("media: legacy cleanup has no ownership receipt")
	}
	jr := j.cfg.Journal.forTenant(item.ref.TenantID)
	var first []uuid.UUID
	for _, operation := range operations {
		var c manifestCommit
		if err := pgx.BeginFunc(ctx, jr.pool, func(tx pgx.Tx) error {
			var err error
			c, err = jr.load(ctx, tx, item.ref.TenantID, operation)
			return err
		}); err != nil {
			return err
		}
		if c.State != "applied" || !c.Ref.Equal(item.ref) || c.Folder != item.prefix {
			return ErrCommitIdentity
		}
		if len(c.Effects.Mirror) != 0 {
			return errors.New("media: legacy retirement cannot copy or overwrite media")
		}
		if receipt := c.Effects.LegacyPublication; receipt != nil {
			if c.Actor != "migration:reader-metadata" {
				return ErrCommitIdentity
			}
			parent, err := j.cfg.Registry.Kind(receipt.Kind)
			if err != nil || !parent.Versions || item.ref.ContentKind != receipt.Kind+PublishedSuffix || !receipt.Initialized {
				return ErrCommitIdentity
			}
			if receipt.Source != "" {
				if contentref.ValidateID(receipt.Content) != nil || contentref.ValidateID(receipt.Version) != nil || receipt.Source != parent.nestedPrefix(receipt.Content, receipt.Version) || receipt.Source == item.prefix {
					return ErrCommitIdentity
				}
			} else if receipt.PriorOperation != "" {
				priorID, err := uuid.Parse(receipt.PriorOperation)
				if err != nil {
					return ErrCommitIdentity
				}
				var prior manifestCommit
				if err := pgx.BeginFunc(ctx, jr.pool, func(tx pgx.Tx) error {
					var err error
					prior, err = jr.load(ctx, tx, item.ref.TenantID, priorID)
					return err
				}); err != nil {
					return err
				}
				if !prior.Ref.Equal(item.ref) || prior.Folder != item.prefix || !prior.terminal() {
					return ErrCommitIdentity
				}
			} else if receipt.DeletionJob <= 0 {
				return ErrCommitIdentity
			} else if _, err := uuid.Parse(receipt.Incarnation); err != nil {
				return ErrCommitIdentity
			}
			if !receipt.Final {
				first = append(first, operation)
			}
		} else if !c.Effects.Deletion {
			return errors.New("media: legacy cleanup needs a canonical retirement or deletion receipt")
		}
		for _, key := range append(append([]string{}, c.Effects.Private...), c.Effects.Public...) {
			parsed, ok := layout.Parse(key)
			if !ok || parsed.Folder() != item.prefix {
				return fmt.Errorf("media: legacy deletion target leaves retired prefix")
			}
		}
		if err := j.repeatLegacyDeletion(ctx, item, c); err != nil {
			return err
		}
	}
	if len(first) == 0 {
		return nil
	}
	return pgx.BeginFunc(ctx, jr.pool, func(tx pgx.Tx) error {
		changed, err := tx.Exec(ctx, `UPDATE `+jr.table+` SET effects=jsonb_set(effects,'{legacy_publication,final}','true'),updated_at=now()
WHERE tenant_id=$1 AND operation_id=ANY($2) AND effects->'legacy_publication'->>'final'='false'`, item.ref.TenantID, first)
		if err != nil || changed.RowsAffected() == 0 {
			return err
		}
		return jr.scheduleCleanupTx(ctx, tx, item.ref, item.prefix, j.cfg.Now().Add(j.cfg.LateUploadWindow), operations, false)
	})
}

// The retired publication kind has no writable registry entry. Its historical
// manifest may not decode, and is never evidence for deleting current content.
// Exact migration receipts own these keys; a newer writer/adoption invalidates
// that ownership even if an older receipt still sits in the cleanup table.
func (j *Jobs) repeatLegacyDeletion(ctx context.Context, item Item, c manifestCommit) error {
	unlock, err := j.cfg.Locker.Lock(ctx, item.ManifestKey())
	if err != nil {
		return err
	}
	defer unlock()
	jr := j.cfg.Journal.forTenant(item.Ref().TenantID)
	var changed bool
	err = jr.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jr.table+` newer
        JOIN `+jr.table+` retired ON retired.tenant_id=newer.tenant_id AND retired.operation_id=$3
        WHERE newer.tenant_id=$1 AND newer.folder_prefix=$2 AND newer.operation_id<>$3
        AND (newer.state IN('open','prepared','frozen') OR
          (newer.state='applied' AND newer.created_at>retired.created_at
           AND NOT newer.effects ? 'legacy_publication')))`, item.Ref().TenantID, item.Prefix(), c.ID).Scan(&changed)
	if err != nil {
		return err
	}
	if changed {
		return ErrCommitIdentity
	}
	if err := j.manifests.retireAllocations(ctx, item, c.Effects.Private); err != nil {
		return err
	}
	keys := append(append([]string{}, c.Effects.Public...), c.Effects.Private...)
	if err := j.deleteKeys(ctx, keys); err != nil {
		return err
	}
	return nil
}

// prepareLegacyRetirementTx also covers an already-retired publication whose
// old sweep or first-delete timer outlived its publication rows. The exact
// historical journal identity or locked internal deletion job is the proof;
// callers cannot use this path for a current version folder.
func (j *PGJournal) prepareLegacyRetirementTx(ctx context.Context, tx pgx.Tx, reg *Registry, prefix string, jobID int64, incarnation string, due time.Time) (uuid.UUID, error) {
	item, legacy, err := reg.cleanupItem(prefix)
	if err != nil || !legacy {
		return uuid.Nil, ErrCommitIdentity
	}
	j = j.forTenant(item.ref.TenantID)
	var existing uuid.UUID
	err = tx.QueryRow(ctx, `SELECT operation_id FROM `+j.table+` WHERE tenant_id=$1 AND folder_prefix=$2 AND state='applied' AND effects ? 'legacy_publication' LIMIT 1`, item.ref.TenantID, prefix).Scan(&existing)
	if err == nil {
		return existing, j.scheduleCleanupTx(ctx, tx, item.ref, prefix, due, []uuid.UUID{existing}, true)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+j.table+` WHERE tenant_id=$1 AND folder_prefix=$2 AND state IN('open','prepared','frozen'))`, item.ref.TenantID, prefix).Scan(&pending); err != nil {
		return uuid.Nil, err
	}
	if pending {
		return uuid.Nil, ErrCommitPending
	}
	proof := legacyPublication{Kind: strings.TrimSuffix(item.ref.ContentKind, PublishedSuffix), Initialized: true}
	var prior uuid.UUID
	err = tx.QueryRow(ctx, `SELECT operation_id FROM `+j.table+` WHERE tenant_id=$1 AND folder_prefix=$2 AND content_kind=$3 AND content_id=$4 AND content_version_id IS NULL AND state IN('applied','absent') ORDER BY updated_at DESC LIMIT 1`, item.ref.TenantID, prefix, item.ref.ContentKind, item.ref.ContentID).Scan(&prior)
	if err == nil {
		proof.PriorOperation = prior.String()
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	} else {
		if jobID <= 0 {
			return uuid.Nil, errors.New("media: legacy prefix has neither ownership journal nor deletion job")
		}
		if _, err := uuid.Parse(incarnation); err != nil {
			return uuid.Nil, ErrCommitIdentity
		}
		proof.DeletionJob, proof.Incarnation = jobID, incarnation
	}
	rows, err := tx.Query(ctx, `SELECT object_key FROM `+j.allocations+` WHERE tenant_id=$1 AND folder_prefix=$2`, item.ref.TenantID, prefix)
	if err != nil {
		return uuid.Nil, err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return uuid.Nil, err
	}
	keys = append(keys, item.ManifestKey())
	effects, err := json.Marshal(journalEffects{Cleanup: true, Private: keys, LegacyPublication: &proof})
	if err != nil {
		return uuid.Nil, err
	}
	id := uuid.New()
	fingerprint := sha256.Sum256([]byte("legacy-publication-retirement:" + prefix))
	_, err = tx.Exec(ctx, `INSERT INTO `+j.table+`(tenant_id,operation_id,content_kind,content_id,folder_prefix,actor_id,fingerprint,lease_id,state,effects)
VALUES($1,$2,$3,$4,$5,'migration:reader-metadata',$6,$7,'applied',$8)`, item.ref.TenantID, id, item.ref.ContentKind, item.ref.ContentID, prefix, fingerprint[:], uuid.New(), effects)
	if err != nil {
		return uuid.Nil, err
	}
	return id, j.scheduleCleanupTx(ctx, tx, item.ref, prefix, due, []uuid.UUID{id}, true)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func prepareOrphanedLegacyCleanup(ctx context.Context, tx pgx.Tx, reg *Registry, journal *PGJournal, grace time.Duration) error {
	for _, tenant := range reg.Namespaces() {
		var kinds []string
		for _, k := range reg.all {
			if k.ns == tenant && k.Versions {
				kinds = append(kinds, k.Name+PublishedSuffix)
			}
		}
		if len(kinds) == 0 {
			continue
		}
		jr := journal.forTenant(tenant)
		rows, err := tx.Query(ctx, `SELECT d.folder_prefix,CASE WHEN isfinite(d.due_at) THEN d.due_at ELSE $3 END FROM `+jr.cleanup+` d
WHERE d.tenant_id=$1 AND d.content_kind=ANY($2) AND NOT EXISTS(SELECT 1 FROM `+jr.table+` c WHERE c.tenant_id=d.tenant_id AND c.folder_prefix=d.folder_prefix AND c.effects ? 'legacy_publication')`, tenant, kinds, time.Now().Add(grace))
		if err != nil {
			return err
		}
		type pending struct {
			Prefix string
			Due    time.Time
		}
		pendingRows, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pending, error) {
			var p pending
			err := r.Scan(&p.Prefix, &p.Due)
			return p, err
		})
		if err != nil {
			return err
		}
		for _, p := range pendingRows {
			if _, err := journal.prepareLegacyRetirementTx(ctx, tx, reg, p.Prefix, 0, "", maxTime(p.Due, time.Now().Add(grace))); err != nil {
				return err
			}
		}
	}
	return nil
}
