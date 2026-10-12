package groups

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// Scopes provisions the AuthKit groups (scopes) of auth_scope group kinds:
// adapters/authkit over an AuthKit client. Both methods are idempotent.
type Scopes interface {
	// Provision makes each scope a live AuthKit group of its kind's persona,
	// seeded with Owner as its owner when new; a scope retired before is
	// replaced by a new group under the same id.
	Provision(ctx context.Context, scopes []Scope) error
	// Retire stops each scope resolving and granting.
	Retire(ctx context.Context, ids []string) error
}

// Scope is one group's AuthKit scope: ContentKit keys it by the group's id.
type Scope struct {
	ID    string
	Kind  string
	Owner string
}

// scopeBatch bounds the groups one SyncScopes transaction locks.
const scopeBatch = 100

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

// queueScopes queues the scope sync of ids in the writing transaction, when
// River is bound and a Scopes provider is set.
func (s *Store) queueScopes(ctx context.Context, q Querier, ids []string) error {
	client := s.river.bound()
	tx, ok := q.(pgx.Tx)
	if s.scopes == nil || client == nil || !ok || len(ids) == 0 {
		return nil
	}
	_, err := client.InsertTx(ctx, tx, scopeArgs{Tenant: s.tenant, Groups: ids}, &river.InsertOpts{Queue: s.queue, MaxAttempts: 10})
	return err
}

// syncAfter syncs the scopes a committed write changed, best effort (the
// queued job retries), and answers gs as they then stand. Bound to a
// caller's transaction it leaves them to the job.
func (s *Store) syncAfter(ctx context.Context, gs []Group) ([]Group, error) {
	if s.scopes == nil || s.tx != nil {
		return gs, nil
	}
	var ids []GroupID
	for _, g := range gs {
		active := g.State == StateActive
		if active && g.AuthScope == "" && s.cfg.GroupKinds[g.Kind].AuthScope || !active && g.AuthScope != "" {
			ids = append(ids, g.ID)
		}
	}
	if len(ids) == 0 {
		return gs, nil
	}
	if err := s.SyncScopes(ctx, ids); err != nil {
		s.log.WarnContext(ctx, "groups: scope sync failed; the job retries", "err", err)
		return gs, nil
	}
	list := make([]string, len(ids))
	for i, id := range ids {
		list[i] = string(id)
	}
	var now map[GroupID]Group
	if err := s.read(ctx, func(q Querier) (err error) {
		now, err = s.byIDs(ctx, q, list)
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
		n := min(len(list), scopeBatch)
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

// ReconcileScopes syncs every group whose scope AuthKit does not match yet:
// those whose job was lost, or whose sync keeps failing.
func (s *Store) ReconcileScopes(ctx context.Context) error {
	if s.scopes == nil {
		return nil
	}
	var kinds []string
	for name, gk := range s.cfg.GroupKinds {
		if gk.AuthScope {
			kinds = append(kinds, name)
		}
	}
	var errs []error
	for after := ""; ; {
		var ids []GroupID
		rows, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT id FROM %s WHERE tenant_id = $1 AND id > $3
 AND (state = 'active' AND auth_scope IS NULL AND kind = ANY($2::text[]) OR state <> 'active' AND auth_scope IS NOT NULL)
 ORDER BY id LIMIT %d`, s.table("content_groups"), scopeBatch), s.tenant, kinds, after)
		if err == nil {
			ids, err = pgx.CollectRows(rows, pgx.RowTo[GroupID])
		}
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		if len(ids) == 0 {
			return errors.Join(errs...)
		}
		if err := s.SyncScopes(ctx, ids); err != nil {
			errs = append(errs, err)
		}
		after = string(ids[len(ids)-1])
	}
}
