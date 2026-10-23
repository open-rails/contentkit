package entities

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/gate"
)

// Scopes keeps the AuthKit groups (scopes) of auth_scope entity kinds:
// adapters/authkit over an AuthKit client. Provision and Retire are
// idempotent.
type Scopes interface {
	// Provision makes each scope a live AuthKit group of its kind's persona,
	// seeded with Owner as its owner when new; a scope retired before is
	// replaced by a new entity under the same id.
	Provision(ctx context.Context, scopes []Scope) error
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
	ID    string
	Kind  string
	Owner string
}

// Folders brings entities' media folders in line with their state in a
// writing transaction: ExposeTx hides a deleted entity's and shows a restored
// one's, DeleteTx deletes them.
type Folders interface {
	ExposeTx(ctx context.Context, tx pgx.Tx, entities []Ref) error
	DeleteTx(ctx context.Context, tx pgx.Tx, entities []Ref) error
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

func (s *Store) needsReconcile() bool {
	return s.needsSync() || s.folders != nil && len(s.userKinds()) > 0
}

// queueSync queues the sync of ids in the writing transaction, when River
// is bound.
func (s *Store) queueSync(ctx context.Context, q Querier, ids []string) error {
	client := s.river.bound()
	tx, ok := q.(pgx.Tx)
	if !s.needsSync() || client == nil || !ok || len(ids) == 0 {
		return nil
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
				provision, provided = append(provision, Scope{ID: id, Kind: kind, Owner: owner}), append(provided, id)
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

// PurgeFolders deletes the media folders of user-owned entities deleted for
// longer than the retention (Options.DeletedRetention), once: a restore
// after it finds them empty. It returns how many it purged.
func (s *Store) PurgeFolders(ctx context.Context) (int, error) {
	if s.folders == nil {
		return 0, nil
	}
	total := 0
	for {
		n := 0
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT id, kind FROM %s WHERE tenant_id = $1 AND state = 'deleted' AND media_purged_at IS NULL
 AND deleted_at < $2 AND kind = ANY($3::text[]) ORDER BY id LIMIT %d FOR UPDATE SKIP LOCKED`, s.table("content_entities"), syncBatch),
				s.tenant, time.Now().Add(-s.keep), s.userKinds())
			if err != nil {
				return err
			}
			var refs []Ref
			var ids []string
			for rows.Next() {
				var r Ref
				if err := rows.Scan(&r.ID, &r.Kind); err != nil {
					rows.Close()
					return err
				}
				refs, ids = append(refs, r), append(ids, string(r.ID))
			}
			rows.Close()
			if err := rows.Err(); err != nil || len(refs) == 0 {
				return err
			}
			if err := s.folders.DeleteTx(ctx, tx, refs); err != nil {
				return err
			}
			n = len(refs)
			_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET media_purged_at = now() WHERE tenant_id = $1 AND id = ANY($2::text[])`,
				s.table("content_entities")), s.tenant, ids)
			return err
		})
		if total += n; err != nil || n == 0 {
			return total, err
		}
	}
}

// Reconcile syncs every entity what follows from its state does not match
// yet: those whose job was lost, or whose sync keeps failing. It purges the
// media of entities deleted past the retention too (PurgeFolders).
func (s *Store) Reconcile(ctx context.Context) error {
	var errs []error
	if s.scopes != nil {
		var kinds []string
		for name, gk := range s.cfg.EntityKinds {
			if gk.AuthScope {
				kinds = append(kinds, name)
			}
		}
		for after := ""; ; {
			var ids []EntityID
			rows, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT id FROM %s WHERE tenant_id = $1 AND id > $3
 AND (state = 'active' AND auth_scope IS NULL AND kind = ANY($2::text[]) OR state <> 'active' AND auth_scope IS NOT NULL)
 ORDER BY id LIMIT %d`, s.table("content_entities"), syncBatch), s.tenant, kinds, after)
			if err == nil {
				ids, err = pgx.CollectRows(rows, pgx.RowTo[EntityID])
			}
			if err != nil {
				errs = append(errs, err)
				break
			}
			if len(ids) == 0 {
				break
			}
			if err := s.SyncScopes(ctx, ids); err != nil {
				errs = append(errs, err)
			}
			after = string(ids[len(ids)-1])
		}
	}
	_, err := s.PurgeFolders(ctx)
	return errors.Join(append(errs, s.followOwned(ctx, nil), err)...)
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
		if gk.Owner == "user" && gk.AuthScope && (kind == "" || kind == name) {
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
