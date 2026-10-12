package offers

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/gate"
)

// Sale is a key's purchasable offer as a paywall shows it: its terms and the
// price billing checks out.
type Sale struct {
	Key     access.Key `json:"key"`
	Target  Target     `json:"target"`
	Terms   Terms      `json:"terms"`
	PriceID string     `json:"price_id,omitempty"`
}

// Sales answers, by key string, the purchasable offers selling keys (an
// access.Decision's Sell): active offers OpenRails sells on their current
// terms, a paid one with its price. Tier keys and keys sold on terms not yet
// synced have none.
func (s *Service) Sales(ctx context.Context, keys []string) (map[string]Sale, error) {
	var parsed []access.Key
	for _, raw := range keys {
		if k, err := access.ParseKey(raw); err == nil {
			parsed = append(parsed, k)
		}
	}
	live, err := s.Live(ctx, parsed)
	if err != nil {
		return nil, err
	}
	out := map[string]Sale{}
	for k, o := range live {
		if o.Synced && (o.PriceID != "" || o.Terms.Amount == 0) {
			out[k] = Sale{Key: o.Key, Target: o.Target, Terms: o.Terms, PriceID: o.PriceID}
		}
	}
	return out, nil
}

// Current answers, per target in order, its latest offer (active, else the
// last withdrawn) when actor may change its offers (Authorize); nil when they
// may not or it never had one.
func (s *Service) Current(ctx context.Context, a auth.Authenticator, actor access.Actor, targets []Target) ([]*Offer, error) {
	out := make([]*Offer, len(targets))
	if len(targets) == 0 || actor.Anonymous {
		return out, nil
	}
	ok, err := s.editors(ctx, a, actor, targets)
	if err != nil {
		return nil, err
	}
	var kinds, ids []string
	for i, t := range targets {
		if ok[i] {
			kinds, ids = append(kinds, string(t.Kind)), append(ids, t.ID)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	list, err := s.query(ctx, `(o.target_kind, o.target_id) IN (SELECT * FROM unnest($2::text[], $3::text[]))`, kinds, ids)
	if err != nil {
		return nil, err
	}
	// UUIDv7 ids order by creation; an active offer wins over withdrawn ones.
	slices.SortFunc(list, func(a, b Offer) int {
		if (a.State == Active) != (b.State == Active) {
			if a.State == Active {
				return -1
			}
			return 1
		}
		return -compare(a.ID, b.ID)
	})
	for i, t := range targets {
		if !ok[i] {
			continue
		}
		if j := slices.IndexFunc(list, func(o Offer) bool { return o.Target.Kind == t.Kind && o.Target.ID == t.ID }); j >= 0 {
			o := list[j]
			out[i] = &o
		}
	}
	return out, nil
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// editors reports, per target in order, whether actor may change its offers:
// staff holding access.OffersWrite in a's root scope, else a creator of the
// content, or an editor of the user-owned group sold or owning the content:
// its owner, or a holder of access.GroupOffersWrite or
// access.GroupSettingsManage in its AuthKit scope. A missing target is
// ErrNotFound.
func (s *Service) editors(ctx context.Context, a auth.Authenticator, actor access.Actor, targets []Target) ([]bool, error) {
	out := make([]bool, len(targets))
	if actor.Anonymous || len(targets) == 0 {
		return out, nil
	}
	staff, err := gate.Can(ctx, a, actor, string(access.OffersWrite))
	if err != nil {
		return nil, err
	}
	if staff {
		for i := range out {
			out[i] = true
		}
		return out, nil
	}
	type facts struct {
		found, creator     bool
		kind, owner, scope string
	}
	got := make([]facts, len(targets))
	var contents, versions, groups []string
	var contentAt, groupAt []int
	for i, t := range targets {
		if t.Kind == GroupTarget {
			groups, groupAt = append(groups, t.ID), append(groupAt, i)
		} else {
			contents, versions, contentAt = append(contents, orID(t)), append(versions, versionOf(t)), append(contentAt, i)
		}
	}
	scan := func(sql string, at []int, args ...any) error {
		if len(at) == 0 {
			return nil
		}
		rows, err := s.pool.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		return forEach(rows, func(r pgx.Rows) error {
			var n int
			var f facts
			if err := r.Scan(&n, &f.creator, &f.kind, &f.owner, &f.scope); err != nil {
				return err
			}
			f.found = true
			got[at[n-1]] = f
			return nil
		})
	}
	if err := scan(`SELECT r.n, EXISTS (SELECT 1 FROM `+s.t.creators+` cr WHERE cr.tenant_id = c.tenant_id AND cr.content_id = c.id AND cr.user_id = $4),
			coalesce(g.kind, ''), coalesce(g.owner_user_id, ''), coalesce(g.auth_scope, '')
		FROM unnest($2::text[], $3::text[]) WITH ORDINALITY AS r(content_id, version_id, n)
		JOIN `+s.t.items+` c ON c.tenant_id = $1 AND c.id = r.content_id
		LEFT JOIN `+s.t.groups+` g ON g.tenant_id = c.tenant_id AND g.id = c.owner_group_id AND g.state = 'active'
		WHERE r.version_id = '' OR EXISTS (SELECT 1 FROM `+s.t.versions+` v WHERE v.tenant_id = c.tenant_id AND v.content_id = c.id AND v.id = r.version_id)`,
		contentAt, s.tenant, contents, versions, actor.ID); err != nil {
		return nil, err
	}
	if err := scan(`SELECT r.n, false, g.kind, coalesce(g.owner_user_id, ''), coalesce(g.auth_scope, '')
		FROM unnest($2::text[]) WITH ORDINALITY AS r(id, n) JOIN `+s.t.groups+` g ON g.tenant_id = $1 AND g.id = r.id`, groupAt, s.tenant, groups); err != nil {
		return nil, err
	}
	asked := map[facts]bool{}
	for i, f := range got {
		if !f.found {
			return nil, fmt.Errorf("%w: %s %s", ErrNotFound, targets[i].Kind, targets[i].ID)
		}
		if out[i] = f.creator && actor.Person(); out[i] || s.kinds.GroupKinds[f.kind].Owner != "user" {
			continue
		}
		g := facts{kind: f.kind, owner: f.owner, scope: f.scope}
		ok, seen := asked[g]
		if !seen {
			if ok, err = gate.GroupEditor(ctx, a, actor, f.owner, f.scope, access.GroupOffersWrite(f.kind), access.GroupSettingsManage(f.kind)); err != nil {
				return nil, err
			}
			asked[g] = ok
		}
		out[i] = ok
	}
	return out, nil
}

func forEach(rows pgx.Rows, fn func(pgx.Rows) error) error {
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
