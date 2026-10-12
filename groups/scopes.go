package groups

import (
	"context"
	"errors"
	"fmt"
	"slices"

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
	// Holding lists the live scopes of the kinds' personas in which user
	// holds the kind's access.GroupSettingsManage.
	Holding(ctx context.Context, user string, kinds []string) ([]string, error)
}

// Scope is one group's AuthKit scope: ContentKit keys it by the group's id.
type Scope struct {
	ID    string
	Kind  string
	Owner string
}

// Folders deletes groups' media folders in a writing transaction.
type Folders interface {
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
	if s.scopes == nil && s.purge == nil || client == nil || !ok || len(ids) == 0 {
		return nil
	}
	_, err := client.InsertTx(ctx, tx, syncArgs{Tenant: s.tenant, Groups: ids}, &river.InsertOpts{Queue: s.queue, MaxAttempts: 10})
	return err
}

// syncAfter syncs the groups a committed write changed, best effort (the
// queued job retries), and answers gs as they then stand. Bound to a
// caller's transaction it leaves them to the job.
func (s *Store) syncAfter(ctx context.Context, gs []Group, changed []string) ([]Group, error) {
	if s.tx != nil || len(changed) == 0 || s.scopes == nil && s.purge == nil {
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
// AuthKit scope (SyncScopes) and, for a deleted user-owned group, the
// deletion of the content it owns (Options.DeleteContents). Repeating it
// changes nothing.
func (s *Store) Sync(ctx context.Context, ids []GroupID) error {
	if err := s.SyncScopes(ctx, ids); err != nil {
		return err
	}
	list, err := uniqueIDs(ids)
	if err != nil {
		return err
	}
	return s.cascade(ctx, list)
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

// cascade deletes the live content owned by those of groups that are
// deleted user-owned groups (every one with nil groups), through
// Options.DeleteContents, a batch at a time.
func (s *Store) cascade(ctx context.Context, groups []string) error {
	if s.purge == nil {
		return nil
	}
	for after := ""; ; {
		rows, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT c.id FROM %s c JOIN %s g ON g.tenant_id = c.tenant_id AND g.id = c.owner_group_id
 WHERE c.tenant_id = $1 AND g.state = 'deleted' AND g.kind = ANY($2::text[]) AND ($3::text[] IS NULL OR g.id = ANY($3::text[]))
   AND c.state <> 'deleted' AND c.id > $4 ORDER BY c.id LIMIT %d`, s.table("content_items"), s.table("content_groups"), syncBatch),
			s.tenant, s.userKinds(), groups, after)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(ids) == 0 {
			return err
		}
		if err := s.purge(ctx, ids); err != nil {
			return fmt.Errorf("groups: delete owned content: %w", err)
		}
		after = ids[len(ids)-1]
	}
}

// Reconcile syncs every group what follows from its state does not match
// yet: those whose job was lost, or whose sync keeps failing.
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
	return errors.Join(append(errs, s.cascade(ctx, nil))...)
}

// heldScopes is the live scopes of user-owned auth_scope kinds (of kind,
// when set) in which user holds the kind's settings permission.
func (s *Store) heldScopes(ctx context.Context, user, kind string) ([]string, error) {
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
	return s.scopes.Holding(ctx, user, kinds)
}
