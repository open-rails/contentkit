package media

import (
	"context"
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

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
