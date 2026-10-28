package entitystore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	entities "github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/gate"
)

// Folders records durable retirement and reports its actual storage outcome.
// PurgeRetired is invoked only by the existing content-deletion sweep.
type Folders interface {
	RetireTx(context.Context, pgx.Tx, []entities.Ref) error
	RetirementStartedTx(context.Context, pgx.Tx, entities.Ref) (bool, error)
	RetirementComplete(context.Context, entities.Ref) (bool, error)
	PurgeRetired(context.Context, int) (int, error)
}

// syncBatch bounds the entities one sync transaction locks.
const syncBatch = 100

// unscoped lists the entities among gs that need a scope provisioned.
func (s *Store) unscoped(gs []entities.Entity) []string {
	var out []string
	for _, g := range gs {
		if s.cfg.EntityKinds[g.Kind].AuthScope && g.AuthScope == "" && g.State == entities.StateActive {
			out = append(out, string(g.ID))
		}
	}
	return out
}

// syncAfter finishes committed channel changes synchronously. A caller-owned
// transaction (an import) must call FinishChannels after committing it.
func (s *Store) syncAfter(ctx context.Context, gs []entities.Entity, changed []string) ([]entities.Entity, error) {
	if s.tx != nil || !s.channels || len(changed) == 0 || s.cfg.Channels == nil {
		return gs, nil
	}
	ids := make([]entities.EntityID, len(changed))
	for i, id := range changed {
		ids[i] = entities.EntityID(id)
	}
	if err := s.FinishChannels(ctx, ids); err != nil {
		return gs, err
	}
	var now map[entities.EntityID]entities.Entity
	if err := s.read(ctx, func(q Querier) (err error) { now, err = s.byIDs(ctx, q, changed); return err }); err != nil {
		return nil, err
	}
	out := slices.Clone(gs)
	for i, g := range out {
		if n, ok := now[g.ID]; ok {
			out[i] = n
		}
	}
	return out, nil
}

// FinishChannels completes the external effects of committed channel writes.
// It is safe to retry after a failed request, including a lost response from
// AuthKit. Calls run outside SQL transactions, so embedded and remote providers
// share the same contract. It adds no job, timer or separate retry record.
func (s *Store) FinishChannels(ctx context.Context, ids []entities.EntityID) error {
	if s.tx != nil {
		return errors.New("channels: finish requires a committed transaction")
	}
	list, err := uniqueIDs(ids)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return nil
	}
	rows, err := s.pool.Query(ctx, "SELECT id FROM "+s.table("content_entities")+" WHERE tenant_id=$1 AND kind='channel' AND id=ANY($2::text[]) ORDER BY id", s.tenant, list)
	if err != nil {
		return err
	}
	list, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(list) == 0 {
		return err
	}
	if s.scopes == nil {
		return errors.New("channels: authorization group provider is required")
	}
	if s.owned != nil {
		if err := s.owned.DeleteOwned(ctx, list); err != nil {
			return fmt.Errorf("channels: delete owned content: %w", err)
		}
	}
	for _, id := range list {
		if err := s.finishChannelScope(ctx, id); err != nil {
			return err
		}
	}
	if s.owned != nil {
		if err := s.owned.RestoreOwned(ctx, list); err != nil {
			return fmt.Errorf("channels: restore owned content: %w", err)
		}
	}
	return nil
}

type channelState struct {
	state        entities.State
	owner, scope string
	revision     int64
}

// finishChannelScope follows the stored state, rechecking after each external
// call. A concurrent edit makes this request follow the new state or return a
// retryable conflict. Neither an unavailable provider nor a stale acknowledgment
// grants a pending/deleted channel local authority.
func (s *Store) finishChannelScope(ctx context.Context, id string) error {
	for attempt := 0; attempt < 4; attempt++ {
		var before channelState
		err := s.pool.QueryRow(ctx, "SELECT state,coalesce(owner_user_id,''),coalesce(auth_scope,''),revision FROM "+s.table("content_entities")+" WHERE tenant_id=$1 AND id=$2 AND kind='channel'", s.tenant, id).Scan(&before.state, &before.owner, &before.scope, &before.revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if before.state == entities.StateActive && before.scope != "" {
			return nil
		}
		if before.state == entities.StateActive {
			err = s.scopes.Provision(ctx, []entities.Scope{{ID: id, Kind: "channel", Owner: before.owner, Restore: true}})
		} else {
			// Retire even a locally pending ID: a prior provider call may have
			// committed before its response was lost.
			target := before.scope
			if target == "" {
				target = id
			}
			err = s.scopes.Retire(ctx, []string{target})
		}
		// Provider errors can be lost acknowledgements after the remote write.
		// Still inspect concurrent state before returning the original error.
		providerErr := err
		done := false
		staleActive := false
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			var after channelState
			err := tx.QueryRow(ctx, "SELECT state,coalesce(owner_user_id,''),coalesce(auth_scope,''),revision FROM "+s.table("content_entities")+" WHERE tenant_id=$1 AND id=$2 AND kind='channel' FOR UPDATE", s.tenant, id).Scan(&after.state, &after.owner, &after.scope, &after.revision)
			if errors.Is(err, pgx.ErrNoRows) {
				done = true
				return nil
			}
			if err != nil {
				return err
			}
			if after != before {
				// A late retirement may have invalidated a concurrent restore.
				// Commit a pending marker, then report conflict outside this tx.
				if after.state == entities.StateActive {
					if after.scope != "" {
						if _, err := tx.Exec(ctx, "UPDATE "+s.table("content_entities")+" SET auth_scope=NULL,revision=revision+1,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2", s.tenant, id); err != nil {
							return err
						}
					}
					staleActive = true
				}
				return nil
			}
			if providerErr != nil {
				return nil
			}
			desired := ""
			if after.state == entities.StateActive {
				desired = id
			}
			if after.scope != desired {
				if _, err := tx.Exec(ctx, "UPDATE "+s.table("content_entities")+" SET auth_scope=nullif($3,''),revision=revision+1,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2", s.tenant, id, desired); err != nil {
					return err
				}
			}
			done = true
			return nil
		})
		if providerErr != nil {
			return errors.Join(fmt.Errorf("channels: apply authorization group for %s: %w", id, providerErr), err)
		}
		if err != nil {
			return err
		}
		if staleActive {
			return fmt.Errorf("%w: channel changed during authorization update; retry the current channel operation", ErrConflict)
		}
		if done {
			return nil
		}
	}
	return fmt.Errorf("%w: channel changed while applying authorization; retry", ErrConflict)
}

func (s *Store) userKinds() []string {
	if s.cfg.Channels != nil {
		return []string{"channel"}
	}
	return nil
}

// PurgeFolders claims expired entity media, runs the shared retirement pass,
// and marks only physically completed folders. Domain rows remain retryable
// after a storage failure. The catalog deletion sweep is its sole scheduler.
func (s *Store) PurgeFolders(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, errors.New("entities: purge limit must be positive")
	}
	if s.folders == nil {
		return 0, nil
	}
	cutoff := time.Now().Add(-s.keep)
	batch := min(syncBatch, limit)
	// Keysets visit each eligible entity once per phase, including failed ones.
	// The existing job context bounds the pass; storage attempt counts must not
	// stop later entity IDs from receiving their durable claim.
	walk := func(visit func([]entities.Ref) error) error {
		after := ""
		for {
			rows, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT id,kind FROM %s WHERE tenant_id=$1 AND state='deleted' AND media_purged_at IS NULL
AND deleted_at<$2 AND kind=ANY($3::text[]) AND id>$4 ORDER BY id LIMIT %d`, s.table("content_entities"), batch), s.tenant, cutoff, s.userKinds(), after)
			if err != nil {
				return err
			}
			refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (entities.Ref, error) {
				var r entities.Ref
				err := row.Scan(&r.ID, &r.Kind)
				return r, err
			})
			if err != nil || len(refs) == 0 {
				return err
			}
			if err := visit(refs); err != nil {
				return err
			}
			after = string(refs[len(refs)-1].ID)
			if len(refs) < batch {
				return nil
			}
		}
	}
	var failures error
	err := walk(func(refs []entities.Ref) error {
		for _, ref := range refs {
			err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
				var one int
				err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT 1 FROM %s WHERE tenant_id=$1 AND id=$2 AND state='deleted' AND media_purged_at IS NULL AND deleted_at<$3 FOR UPDATE SKIP LOCKED`, s.table("content_entities")), s.tenant, string(ref.ID), cutoff).Scan(&one)
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				if err != nil {
					return err
				}
				started, err := s.folders.RetirementStartedTx(ctx, tx, ref)
				if err != nil || started {
					return err
				}
				return s.folders.RetireTx(ctx, tx, []entities.Ref{ref})
			})
			failures = errors.Join(failures, err)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		return nil
	})
	failures = errors.Join(failures, err)
	if ctx.Err() != nil {
		return 0, errors.Join(failures, ctx.Err())
	}
	// Exactly one shared storage pass: retries cannot restart the same failing
	// namespace for every entity batch. Poll and account intents join this pass.
	_, err = s.folders.PurgeRetired(ctx, limit)
	failures = errors.Join(failures, err)
	total := 0
	err = walk(func(refs []entities.Ref) error {
		var complete []string
		for _, ref := range refs {
			done, err := s.folders.RetirementComplete(ctx, ref)
			if err != nil {
				failures = errors.Join(failures, err)
				continue
			}
			if done {
				complete = append(complete, string(ref.ID))
			}
		}
		if len(complete) == 0 {
			return ctx.Err()
		}
		result, err := s.pool.Exec(ctx, fmt.Sprintf(`UPDATE %s SET media_purged_at=now() WHERE tenant_id=$1 AND id=ANY($2::text[]) AND state='deleted' AND media_purged_at IS NULL AND deleted_at<$3`, s.table("content_entities")), s.tenant, complete, cutoff)
		if err != nil {
			return err
		}
		total += int(result.RowsAffected())
		return nil
	})
	return total, errors.Join(failures, err)
}

func (s *Store) checkMediaRestorable(ctx context.Context, tx pgx.Tx, ref entities.Ref) error {
	if s.folders == nil {
		return nil
	}
	started, err := s.folders.RetirementStartedTx(ctx, tx, ref)
	if err != nil {
		return err
	}
	if started {
		return fmt.Errorf("%w: entity %s media retirement is irreversible", ErrConflict, ref.ID)
	}
	return nil
}

// heldScopes is, per live scope of user-owned auth_scope kinds (of kind,
// when set), the entity permissions the actor can exercise. Directory grants
// are candidates; scoped requests also retain their credential's ceilings.
func (s *Store) heldScopes(ctx context.Context, actor access.Actor, kind string) (map[string][]string, error) {
	if s.scopes == nil {
		return nil, nil
	}
	var kinds []string
	for name, gk := range s.cfg.EntityKinds {
		if gk.AuthScope && (kind == "" || kind == name) {
			kinds = append(kinds, name)
		}
	}
	if len(kinds) == 0 {
		return nil, nil
	}
	slices.Sort(kinds)
	if local, ok := s.scopes.(entities.LocalScopes); ok {
		var scopeIDs []string
		err := s.read(ctx, func(q Querier) error {
			rows, err := q.Query(ctx, fmt.Sprintf(`SELECT auth_scope FROM %s WHERE tenant_id=$1 AND state='active' AND auth_scope IS NOT NULL AND kind=ANY($2::text[]) ORDER BY auth_scope`, s.table("content_entities")), s.tenant, kinds)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				scopeIDs = append(scopeIDs, id)
			}
			return rows.Err()
		})
		if err != nil {
			return nil, err
		}
		held, err := local.HoldingScopes(ctx, actor.AccountID(), kinds, scopeIDs)
		if err != nil {
			return nil, err
		}
		return s.credentialScopes(ctx, actor, held)
	}
	held, err := s.scopes.Holding(ctx, actor.AccountID(), kinds)
	if err != nil {
		return nil, err
	}
	return s.credentialScopes(ctx, actor, held)
}

// credentialScopes preserves the trusted legacy directory contract while
// intersecting scoped metadata with the actual request's verified authority.
func (s *Store) credentialScopes(ctx context.Context, actor access.Actor, candidates map[string][]string) (map[string][]string, error) {
	if s.as == nil {
		return candidates, nil
	}
	if _, scoped := s.as.auth.(gate.ScopedAuthorization); !scoped {
		return candidates, nil
	}
	held := make(map[string][]string)
	for scope, permissions := range candidates {
		for _, permission := range permissions {
			allowed, err := gate.ChannelEditor(ctx, s.as.auth, actor, scope, permission)
			if err != nil {
				return nil, err
			}
			if allowed {
				held[scope] = append(held[scope], permission)
			}
		}
	}
	return held, nil
}
