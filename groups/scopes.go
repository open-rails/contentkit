package groups

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// Scopes keeps the AuthKit groups (scopes) of auth_scope group kinds:
// adapters/authkit over an AuthKit client. Provision and Retire are
// idempotent.
type Scopes interface {
	// Provision makes each scope a live AuthKit group of its kind's persona,
	// seeded with Owner as its owner when new; a scope retired before is
	// replaced by a new group under the same id.
	Provision(ctx context.Context, scopes []Scope) error
	// Retire stops each scope resolving and granting.
	Retire(ctx context.Context, ids []string) error
	// Holding answers, per live scope of the kinds' personas user belongs
	// to, the kind's access.GroupPermissions user holds there.
	Holding(ctx context.Context, user string, kinds []string) (map[string][]string, error)
}

// LocalScopes optionally narrows authorization lookups to scope IDs verified
// by this app's store. It lets a shared identity directory resolve grants
// without enumerating or trusting groups belonging to other apps.
type LocalScopes interface {
	HoldingScopes(ctx context.Context, user string, kinds, scopeIDs []string) (map[string][]string, error)
}

// AppBinding identifies the tenant/app an authorization scope provider serves.
// A scoped engine requires a matching binding before enabling auth_scope kinds.
type AppBinding interface {
	ContentKitScope() (tenantID, appID string)
}

// Scope is one group's AuthKit scope: ContentKit keys it by the group's id.
type Scope struct {
	ID    string
	Kind  string
	Owner string
}

// Folders brings groups' media folders in line with their state in a
// writing transaction: ExposeTx hides a deleted group's and shows a restored
// one's, DeleteTx deletes them.
type Folders interface {
	ExposeTx(ctx context.Context, tx pgx.Tx, groups []Ref) error
	DeleteTx(ctx context.Context, tx pgx.Tx, groups []Ref) error
}

// Ref names one group with its kind.
type Ref struct {
	Kind string
	ID   GroupID
}

// syncBatch bounds the groups one sync transaction locks.
const syncBatch = 100

// unscoped lists the groups among gs that need a scope provisioned.
func (s *Store) unscoped(gs []Group) []string {
	var out []string
	for _, g := range gs {
		if s.cfg.GroupKinds[g.Kind].AuthScope && g.AuthScope == "" && g.State == StateActive {
			out = append(out, string(g.ID))
		}
	}
	return out
}

// queueSync queues the sync of ids in the writing transaction, when River
// is bound.
func (s *Store) queueSync(ctx context.Context, q Querier, ids []string) error {
	client := s.river.bound()
	tx, ok := q.(pgx.Tx)
	if s.scopes == nil && s.owned == nil || client == nil || !ok || len(ids) == 0 {
		return nil
	}
	_, err := client.InsertTx(ctx, tx, syncArgs{Tenant: s.tenant, Groups: ids}, &river.InsertOpts{Queue: s.queue, MaxAttempts: 10})
	return err
}

// syncAfter syncs the groups a committed write changed, best effort (the
// queued job retries), and answers gs as they then stand. Bound to a
// caller's transaction it leaves them to the job.
func (s *Store) syncAfter(ctx context.Context, gs []Group, changed []string) ([]Group, error) {
	if s.tx != nil || len(changed) == 0 || s.scopes == nil && s.owned == nil {
		return gs, nil
	}
	ids := make([]GroupID, len(changed))
	for i, id := range changed {
		ids[i] = GroupID(id)
	}
	if err := s.Sync(ctx, ids); err != nil {
		s.log.WarnContext(ctx, "groups: sync failed; the job retries", "err", err)
		return gs, nil
	}
	var now map[GroupID]Group
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

// Sync brings what follows from each group's state in line with it: its
// AuthKit scope (SyncScopes) and the content a user-owned group owns,
// deleted with it and restored with it (Options.Owned). Repeating it changes
// nothing.
func (s *Store) Sync(ctx context.Context, ids []GroupID) error {
	if err := s.SyncScopes(ctx, ids); err != nil {
		return err
	}
	list, err := uniqueIDs(ids)
	if err != nil {
		return err
	}
	return s.followOwned(ctx, list)
}

// followOwned deletes and restores the content groups own as they stand;
// nil is every group.
func (s *Store) followOwned(ctx context.Context, groups []string) error {
	if s.owned == nil {
		return nil
	}
	if err := s.owned.DeleteOwned(ctx, groups); err != nil {
		return fmt.Errorf("groups: delete owned content: %w", err)
	}
	if err := s.owned.RestoreOwned(ctx, groups); err != nil {
		return fmt.Errorf("groups: restore owned content: %w", err)
	}
	return nil
}

// SyncScopes brings AuthKit in line with each group: an active group of an
// auth_scope kind without a scope gets one, keyed by its id and owned by its
// owner; a group no longer active has its scope retired and cleared. It
// holds the groups' row locks over the AuthKit calls, so a concurrent delete
// or restore waits and is synced after; repeating it changes nothing.
func (s *Store) SyncScopes(ctx context.Context, ids []GroupID) error {
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
 WHERE tenant_id = $1 AND id = ANY($2::text[]) ORDER BY id FOR UPDATE`, s.table("content_groups")), s.tenant, ids)
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
			case state == StateActive && scope == "" && s.cfg.GroupKinds[kind].AuthScope:
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
				return fmt.Errorf("groups: provision scopes: %w", err)
			}
			if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET auth_scope = id, updated_at = now(), revision = revision + 1
 WHERE tenant_id = $1 AND id = ANY($2::text[])`, s.table("content_groups")), s.tenant, provided); err != nil {
				return err
			}
		}
		if len(retire) > 0 {
			if err := s.scopes.Retire(ctx, retire); err != nil {
				return fmt.Errorf("groups: retire scopes: %w", err)
			}
			if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET auth_scope = NULL, updated_at = now(), revision = revision + 1
 WHERE tenant_id = $1 AND id = ANY($2::text[])`, s.table("content_groups")), s.tenant, retired); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) userKinds() []string {
	var out []string
	for name, gk := range s.cfg.GroupKinds {
		if gk.Owner == "user" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// PurgeFolders deletes the media folders of user-owned groups deleted for
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
 AND deleted_at < $2 AND kind = ANY($3::text[]) ORDER BY id LIMIT %d FOR UPDATE SKIP LOCKED`, s.table("content_groups"), syncBatch),
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
				s.table("content_groups")), s.tenant, ids)
			return err
		})
		if total += n; err != nil || n == 0 {
			return total, err
		}
	}
}

// Reconcile syncs every group what follows from its state does not match
// yet: those whose job was lost, or whose sync keeps failing. It purges the
// media of groups deleted past the retention too (PurgeFolders).
func (s *Store) Reconcile(ctx context.Context) error {
	var errs []error
	if s.scopes != nil {
		var kinds []string
		for name, gk := range s.cfg.GroupKinds {
			if gk.AuthScope {
				kinds = append(kinds, name)
			}
		}
		for after := ""; ; {
			var ids []GroupID
			rows, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT id FROM %s WHERE tenant_id = $1 AND id > $3
 AND (state = 'active' AND auth_scope IS NULL AND kind = ANY($2::text[]) OR state <> 'active' AND auth_scope IS NOT NULL)
 ORDER BY id LIMIT %d`, s.table("content_groups"), syncBatch), s.tenant, kinds, after)
			if err == nil {
				ids, err = pgx.CollectRows(rows, pgx.RowTo[GroupID])
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
// when set) user belongs to, the group permissions user holds there.
func (s *Store) heldScopes(ctx context.Context, user, kind string) (map[string][]string, error) {
	if s.scopes == nil {
		return nil, nil
	}
	var kinds []string
	for name, gk := range s.cfg.GroupKinds {
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
			rows, err := q.Query(ctx, fmt.Sprintf(`SELECT auth_scope FROM %s WHERE tenant_id=$1 AND state='active' AND auth_scope IS NOT NULL AND kind=ANY($2::text[]) ORDER BY auth_scope`, s.table("content_groups")), s.tenant, kinds)
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
		return local.HoldingScopes(ctx, user, kinds, scopeIDs)
	}
	return s.scopes.Holding(ctx, user, kinds)
}
