package entities

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/gate"
)

// ScopeProvisioner ensures the declared authorization groups exist. It is the
// only provider capability needed by explicit fixed-owner migrations.
type ScopeProvisioner interface {
	Provision(ctx context.Context, scopes []Scope) error
}

// Scopes keeps the AuthKit groups of auth_scope entity kinds. Provision and
// Retire are idempotent; runtime adds read/revocation behavior to provisioning.
type Scopes interface {
	ScopeProvisioner
	// Retire stops each scope resolving and granting.
	Retire(ctx context.Context, ids []string) error
	// Holding answers, per live scope of the kinds' personas user belongs
	// to, the kind's access.EntityPermissions user holds there.
	Holding(ctx context.Context, user string, kinds []string) (map[string][]string, error)
}

// LocalScopes optionally narrows authorization lookups to scope IDs verified
// by this app's store. It lets a shared identity directory resolve grants
// without enumerating or trusting entities belonging to other apps.
type LocalScopes interface {
	HoldingScopes(ctx context.Context, user string, kinds, scopeIDs []string) (map[string][]string, error)
}

// AppBinding identifies the tenant/app an authorization scope provider serves.
// A scoped engine requires a matching binding before enabling auth_scope kinds.
type AppBinding interface {
	ContentKitScope() (tenantID, appID string)
}

// Scope is one entity's AuthKit scope: ContentKit keys it by the entity's id.
type Scope struct {
	// Restore permits deliberately restoring a previously retired group. Normal
	// provisioning (including configured fixed owners) preserves live groups
	// and refuses deleted groups rather than silently removing their grants.
	Restore bool
	ID      string
	Kind    string
	Owner   string
}

// Folders records durable retirement and reports its actual storage outcome.
// PurgeRetired is invoked only by the existing content-deletion sweep.
type Folders interface {
	RetireTx(context.Context, pgx.Tx, []Ref) error
	RetirementStartedTx(context.Context, pgx.Tx, Ref) (bool, error)
	RetirementComplete(context.Context, Ref) (bool, error)
	PurgeRetired(context.Context, int) (int, error)
}

// Ref names one entity with its kind.
type Ref struct {
	Kind string
	ID   EntityID
}

// syncBatch bounds the entities one sync transaction locks.
const syncBatch = 100

// unscoped lists the entities among gs that need a scope provisioned.
func (s *Store) unscoped(gs []Entity) []string {
	var out []string
	for _, g := range gs {
		if s.cfg.EntityKinds[g.Kind].AuthScope && g.AuthScope == "" && g.State == StateActive {
			out = append(out, string(g.ID))
		}
	}
	return out
}

// needsSync reports whether configured kinds use the external lifecycle adapters.
// Hosts commonly install Owned for ordinary catalog writes without configuring
// any user-owned kinds; those hosts have no asynchronous ownership work.
func (s *Store) needsSync() bool {
	for _, kind := range s.cfg.EntityKinds {
		if kind.AuthScope && s.scopes != nil || kind.Owner == "user" && s.owned != nil {
			return true
		}
	}
	return false
}

// queueSync records exact lifecycle work in the entity write's transaction.
// An insert-only client makes admission independent of running local workers.
func (s *Store) queueSync(ctx context.Context, q Querier, ids []string) error {
	if !s.needsSync() || len(ids) == 0 {
		return nil
	}
	tx, ok := q.(pgx.Tx)
	if !ok {
		return errors.New("entities: lifecycle synchronization requires a transaction")
	}
	client := s.river.bound()
	if client == nil {
		var err error
		client, err = river.NewClient(riverpgxv5.New(s.pool), &river.Config{Schema: s.riverSchema})
		if err != nil {
			return err
		}
	}
	_, err := client.InsertTx(ctx, tx, syncArgs{Tenant: s.tenant, Entities: ids}, &river.InsertOpts{Queue: s.queue, MaxAttempts: 10})
	return err
}

// syncAfter syncs the entities a committed write changed, best effort (the
// queued job retries), and answers gs as they then stand. Bound to a
// caller's transaction it leaves them to the job.
func (s *Store) syncAfter(ctx context.Context, gs []Entity, changed []string) ([]Entity, error) {
	if s.tx != nil || len(changed) == 0 || !s.needsSync() {
		return gs, nil
	}
	ids := make([]EntityID, len(changed))
	for i, id := range changed {
		ids[i] = EntityID(id)
	}
	if err := s.Sync(ctx, ids); err != nil {
		s.log.WarnContext(ctx, "entities: sync failed; the job retries", "err", err)
		return gs, nil
	}
	var now map[EntityID]Entity
	if err := s.read(ctx, func(q Querier) (err error) {
		now, err = s.byIDs(ctx, q, changed)
		return err
	}); err != nil {
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

// Sync brings what follows from each entity's state in line with it: its
// AuthKit scope (SyncScopes) and the content a user-owned entity owns,
// deleted with it and restored with it (Options.Owned). Repeating it changes
// nothing.
func (s *Store) Sync(ctx context.Context, ids []EntityID) error {
	if err := s.SyncScopes(ctx, ids); err != nil {
		return err
	}
	list, err := uniqueIDs(ids)
	if err != nil {
		return err
	}
	return s.followOwned(ctx, list)
}

// followOwned deletes and restores the content entities own as they stand;
// nil is every entity.
func (s *Store) followOwned(ctx context.Context, entities []string) error {
	if s.owned == nil {
		return nil
	}
	if err := s.owned.DeleteOwned(ctx, entities); err != nil {
		return fmt.Errorf("entities: delete owned content: %w", err)
	}
	if err := s.owned.RestoreOwned(ctx, entities); err != nil {
		return fmt.Errorf("entities: restore owned content: %w", err)
	}
	return nil
}

// SyncScopes brings AuthKit in line with each entity: an active entity of an
// auth_scope kind without a scope gets one, keyed by its id and owned by its
// owner; an entity no longer active has its scope retired and cleared. It
// holds the entities' row locks over the AuthKit calls, so a concurrent delete
// or restore waits and is synced after; repeating it changes nothing.
func (s *Store) SyncScopes(ctx context.Context, ids []EntityID) error {
	if s.scopes == nil {
		return nil
	}
	list, err := uniqueIDs(ids)
	if err != nil {
		return err
	}
	slices.Sort(list)
	for len(list) > 0 {
		n := min(len(list), syncBatch)
		if err := s.syncScopes(ctx, list[:n]); err != nil {
			return err
		}
		list = list[n:]
	}
	return nil
}

func (s *Store) syncScopes(ctx context.Context, ids []string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT id, kind, state, coalesce(owner_user_id, ''), coalesce(auth_scope, '') FROM %s
 WHERE tenant_id = $1 AND id = ANY($2::text[]) ORDER BY id FOR UPDATE`, s.table("content_entities")), s.tenant, ids)
		if err != nil {
			return err
		}
		var provision []Scope
		var retire, provided, retired []string
		for rows.Next() {
			var id, kind, owner, scope string
			var state State
			if err := rows.Scan(&id, &kind, &state, &owner, &scope); err != nil {
				rows.Close()
				return err
			}
			switch {
			case state == StateActive && scope == "" && s.cfg.EntityKinds[kind].AuthScope:
				provision, provided = append(provision, Scope{ID: id, Kind: kind, Owner: owner, Restore: true}), append(provided, id)
			case state != StateActive && scope != "":
				retire, retired = append(retire, scope), append(retired, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(provision) > 0 {
			if err := s.scopes.Provision(ctx, provision); err != nil {
				return fmt.Errorf("entities: provision scopes: %w", err)
			}
			if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET auth_scope = id, updated_at = now(), revision = revision + 1
 WHERE tenant_id = $1 AND id = ANY($2::text[])`, s.table("content_entities")), s.tenant, provided); err != nil {
				return err
			}
		}
		if len(retire) > 0 {
			if err := s.scopes.Retire(ctx, retire); err != nil {
				return fmt.Errorf("entities: retire scopes: %w", err)
			}
			if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET auth_scope = NULL, updated_at = now(), revision = revision + 1
 WHERE tenant_id = $1 AND id = ANY($2::text[])`, s.table("content_entities")), s.tenant, retired); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) userKinds() []string {
	var out []string
	for name, gk := range s.cfg.EntityKinds {
		if gk.Owner == "user" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
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
	walk := func(visit func([]Ref) error) error {
		after := ""
		for {
			rows, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT id,kind FROM %s WHERE tenant_id=$1 AND state='deleted' AND media_purged_at IS NULL
AND deleted_at<$2 AND kind=ANY($3::text[]) AND id>$4 ORDER BY id LIMIT %d`, s.table("content_entities"), batch), s.tenant, cutoff, s.userKinds(), after)
			if err != nil {
				return err
			}
			refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Ref, error) { var r Ref; err := row.Scan(&r.ID, &r.Kind); return r, err })
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
	err := walk(func(refs []Ref) error {
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
				return s.folders.RetireTx(ctx, tx, []Ref{ref})
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
	err = walk(func(refs []Ref) error {
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

func (s *Store) checkMediaRestorable(ctx context.Context, tx pgx.Tx, ref Ref) error {
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
	if local, ok := s.scopes.(LocalScopes); ok {
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
			allowed, err := gate.EntityEditor(ctx, s.as.auth, actor, "", scope, permission)
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
