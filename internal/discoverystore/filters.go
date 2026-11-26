package discoverystore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/domainpolicy"
	entitystore "github.com/open-rails/contentkit/internal/entitystore"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/readerprefs"
	"github.com/open-rails/contentkit/kinds"
)

func guestFilterValue(f discovery.GuestFilterSetting) (string, error) {
	if f.EntityID == "" || (f.Visible == nil) == (f.Default == nil) || f.Default != nil && !*f.Default {
		return "", fmt.Errorf("%w: a preference needs visible or default:true", ErrInvalid)
	}
	if f.Default != nil {
		return readerprefs.Default, nil
	}
	if *f.Visible {
		return readerprefs.Show, nil
	}
	return readerprefs.Hide, nil
}

type filterInventory map[kinds.EntityRef]string

func (s *Service) filterEntities(ctx context.Context) (filterInventory, error) {
	d, _ := s.o.Kinds.DomainFor(gate.Domain(ctx))
	return s.filterEntitiesForDomains(ctx, []kinds.Domain{d})
}

func (s *Service) filterEntitiesForDomains(ctx context.Context, domains []kinds.Domain) (filterInventory, error) {
	out := filterInventory{}
	for _, d := range domains {
		policies, err := domainpolicy.Resolve(ctx, s.o.Pool, s.t.entities, s.o.Tenant, d, s.o.Kinds.ContentFilters.AppliesTo)
		if err != nil {
			return nil, err
		}
		for _, p := range policies {
			for ref, id := range p.ToggleRefs {
				out[ref] = id
			}
		}
	}

	return out, nil
}

func (s *Service) prefs(ctx context.Context, userID string) (map[string]string, error) {
	var raw []byte
	err := s.o.Pool.QueryRow(ctx, `SELECT overrides FROM `+s.t.prefs+` WHERE tenant_id = $1 AND user_id = $2`,
		s.o.Tenant, userID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	overrides := map[string]string{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &overrides); err != nil {
			return nil, err
		}
	}
	return overrides, nil
}

// ContentFilters resolves names from the entity service; policy itself is never
// exposed. Canonical shared choices win over preserved legacy preferences.
func (s *Service) ContentFilters(ctx context.Context, actor access.Actor, language string) (discovery.ContentFilters, error) {
	out := discovery.ContentFilters{AppliesTo: append([]string{}, s.o.Kinds.ContentFilters.AppliesTo...), Filters: []discovery.ContentFilterState{}}
	if language != "" {
		if err := s.checkLanguage(language); err != nil {
			return out, err
		}
	}
	inventory, err := s.filterEntities(ctx)
	if err != nil {
		return out, err
	}
	values := map[string]string{}
	if actor.Member() {
		values, err = s.prefs(ctx, actor.AccountID())
		if err != nil {
			return out, err
		}
	} else if actor.Anonymous {
		choices, err := readerprefs.Overrides(ctx, inventory.allowed())
		if err != nil {
			return out, fmt.Errorf("%w: %s", ErrInvalid, err)
		}
		for k, v := range choices {
			values[k] = v
		}
	}
	var ids []entities.EntityID
	seen := map[string]bool{}
	for _, id := range inventory {
		if !seen[id] {
			ids = append(ids, entities.EntityID(id))
			seen[id] = true
		}
	}
	cards, err := s.o.Entities.Cards(ctx, ids, entitystore.CardOptions{Language: language})
	if err != nil {
		return out, err
	}
	byID := map[string]entities.EntityCard{}
	for _, c := range cards {
		byID[string(c.ID)] = c
	}
	d, _ := s.o.Kinds.DomainFor(gate.Domain(ctx))
	for _, f := range d.Filters {
		id := inventory[f.Entity]
		g, ok := byID[id]
		if !ok {
			continue
		}
		visible, overridden := readerprefs.Stored(values, id)
		if !overridden {
			visible = f.Visible
		}
		out.Filters = append(out.Filters, discovery.ContentFilterState{Entity: g, DefaultVisible: f.Visible, Visible: visible, Overridden: overridden})
	}
	return out, nil
}

func (i filterInventory) allowed() map[string]bool {
	out := map[string]bool{}
	for _, id := range i {
		out[id] = true
	}
	return out
}

func (s *Service) SetContentFilters(ctx context.Context, actor access.Actor, u discovery.ContentFiltersUpdate) error {
	return s.setContentFilters(ctx, actor, u.Filters)
}

// MergeContentFilters carries a signed-out browser's choices into the
// account at sign-in: a choice fills an entity the account has not set, and
// a hide wins over the account's show. Resets and entities no domain offers
// are ignored, so a repeat changes nothing.
func (s *Service) MergeContentFilters(ctx context.Context, actor access.Actor, u discovery.ContentFiltersMerge) error {
	if u.AccountID == "" {
		return fmt.Errorf("%w: account_id is required", ErrInvalid)
	}
	if !actor.Member() || actor.AccountID() != u.AccountID {
		return auth.ErrForbidden
	}
	inventory, err := s.filterEntitiesForDomains(ctx, s.o.Kinds.ContentFilters.Domains)
	if err != nil {
		return err
	}
	allowed := inventory.allowed()
	choices, seen := map[string]bool{}, map[string]bool{}
	for _, f := range u.Filters {
		if _, err := guestFilterValue(f); err != nil {
			return err
		}
		if seen[f.EntityID] {
			return fmt.Errorf("%w: entity %q is set twice", ErrInvalid, f.EntityID)
		}
		seen[f.EntityID] = true
		if f.Visible != nil && allowed[f.EntityID] {
			choices[f.EntityID] = *f.Visible
		}
	}
	if len(choices) == 0 {
		return nil
	}
	return pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.prefs+` (tenant_id,user_id,overrides,updated_at) VALUES ($1,$2,'{}',now()) ON CONFLICT (tenant_id,user_id) DO NOTHING`,
			s.o.Tenant, actor.AccountID()); err != nil {
			return err
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT overrides FROM `+s.t.prefs+` WHERE tenant_id=$1 AND user_id=$2 FOR UPDATE`, s.o.Tenant, actor.AccountID()).Scan(&raw); err != nil {
			return err
		}
		values := map[string]string{}
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		patch := map[string]string{}
		for id, visible := range choices {
			saved, set := readerprefs.Stored(values, id)
			switch {
			case !set && visible:
				patch[readerprefs.Key(id)] = readerprefs.Show
			case !visible && (!set || saved):
				patch[readerprefs.Key(id)] = readerprefs.Hide
			}
		}
		if len(patch) == 0 {
			return nil
		}
		out, err := json.Marshal(patch)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE `+s.t.prefs+` SET overrides=overrides || $3, updated_at=now() WHERE tenant_id=$1 AND user_id=$2`, s.o.Tenant, actor.AccountID(), out)
		return err
	})
}

func (s *Service) setContentFilters(ctx context.Context, actor access.Actor, choices []discovery.GuestFilterSetting) error {
	if !actor.Member() {
		return fmt.Errorf("%w: content filters belong to a signed-in reader", ErrInvalid)
	}
	inventory, err := s.filterEntities(ctx)
	if err != nil {
		return err
	}
	allowed := inventory.allowed()
	patch := map[string]string{}
	seen := map[string]bool{}
	for _, f := range choices {
		value, err := guestFilterValue(f)
		if err != nil {
			return err
		}
		if seen[f.EntityID] {
			return fmt.Errorf("%w: entity %q is set twice", ErrInvalid, f.EntityID)
		}
		seen[f.EntityID] = true
		if !allowed[f.EntityID] {
			return fmt.Errorf("%w: entity %q is not a content filter on this domain", ErrInvalid, f.EntityID)
		}
		patch[readerprefs.Key(f.EntityID)] = value
	}
	return s.patchPrefs(ctx, s.o.Pool, actor.AccountID(), patch)
}

// patchPrefs merges patch into the account's saved choices atomically.
func (s *Service) patchPrefs(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, account string, patch map[string]string) error {
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO `+s.t.prefs+` AS saved(tenant_id,user_id,overrides,updated_at) VALUES($1,$2,$3,now())
 ON CONFLICT(tenant_id,user_id) DO UPDATE SET overrides=saved.overrides || EXCLUDED.overrides,updated_at=now()`, s.o.Tenant, account, raw)
	return err
}

// PolarizingTags are the entities whose galleries have an audience: for now
// the site's declared toggleable content filters, on every domain.
func (s *Service) PolarizingTags(ctx context.Context) ([]string, error) {
	inventory, err := s.filterEntitiesForDomains(ctx, s.o.Kinds.ContentFilters.Domains)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(inventory))
	for _, id := range inventory {
		out = append(out, id)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
