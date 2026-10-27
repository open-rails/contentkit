package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/domainpolicy"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/readerprefs"
	"github.com/open-rails/contentkit/kinds"
)

// ContentFilterState is one configured content filter as the reader has it.
type ContentFilterState struct {
	Entity         entities.EntityCard `json:"entity"`
	DefaultVisible bool                `json:"default_visible"`
	// Visible is the reader's setting, Overridden whether it is their own.
	Visible    bool `json:"visible"`
	Overridden bool `json:"overridden"`
}

// ContentFilters offers one control per entity across the selected content kinds.
type ContentFilters struct {
	AppliesTo      []string             `json:"applies_to"`
	Filters        []ContentFilterState `json:"filters"`
	IncludeDeleted bool                 `json:"include_deleted"`
}

// ContentFiltersUpdate atomically changes only submitted choices. Omitted
// entities and include_deleted retain their values, including across tabs.
type ContentFiltersUpdate struct {
	Filters        []GuestFilterSetting `json:"filters,omitempty"`
	IncludeDeleted *bool                `json:"include_deleted,omitempty"`
}

type ContentFiltersInitialization struct {
	AccountID      string               `json:"account_id"`
	Filters        []GuestFilterSetting `json:"filters"`
	IncludeDeleted bool                 `json:"include_deleted"`
}

// GuestFilterSetting is a shared entity choice or reset without pinning a domain's
// current default value. Exactly one of Visible or Default:true is required.
type GuestFilterSetting struct {
	EntityID string `json:"entity_id"`
	Visible  *bool  `json:"visible,omitempty"`
	Default  *bool  `json:"default,omitempty"`
}

func (f GuestFilterSetting) value() (string, error) {
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

func (s *Service) prefs(ctx context.Context, userID string) (map[string]string, bool, error) {
	var raw []byte
	var includeDeleted bool
	err := s.o.Pool.QueryRow(ctx, `SELECT overrides, include_deleted FROM `+s.t.prefs+` WHERE tenant_id = $1 AND user_id = $2`,
		s.o.Tenant, userID).Scan(&raw, &includeDeleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string]string{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	overrides := map[string]string{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &overrides); err != nil {
			return nil, false, err
		}
	}
	return overrides, includeDeleted, nil
}

// ContentFilters resolves names from the entity service; policy itself is never
// exposed. Canonical shared choices win over preserved legacy preferences.
func (s *Service) ContentFilters(ctx context.Context, actor access.Actor, language string) (ContentFilters, error) {
	out := ContentFilters{AppliesTo: append([]string{}, s.o.Kinds.ContentFilters.AppliesTo...), Filters: []ContentFilterState{}}
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
		values, out.IncludeDeleted, err = s.prefs(ctx, actor.AccountID())
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
	cards, err := s.o.Entities.Cards(ctx, ids, entities.CardOptions{Language: language})
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
		out.Filters = append(out.Filters, ContentFilterState{Entity: g, DefaultVisible: f.Visible, Visible: visible, Overridden: overridden})
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

func (s *Service) SetContentFilters(ctx context.Context, actor access.Actor, u ContentFiltersUpdate) error {
	return s.setContentFilters(ctx, actor, u.Filters, u.IncludeDeleted, false)
}

func (s *Service) InitializeContentFilters(ctx context.Context, actor access.Actor, u ContentFiltersInitialization) error {
	if u.AccountID == "" {
		return fmt.Errorf("%w: account_id is required", ErrInvalid)
	}
	if !actor.Member() || actor.AccountID() != u.AccountID {
		return auth.ErrForbidden
	}
	return s.setContentFilters(ctx, actor, u.Filters, &u.IncludeDeleted, true)
}

func (s *Service) setContentFilters(ctx context.Context, actor access.Actor, choices []GuestFilterSetting, includeDeleted *bool, initialize bool) error {
	if !actor.Member() {
		return fmt.Errorf("%w: content filters belong to a signed-in reader", ErrInvalid)
	}
	var inventory filterInventory
	var err error
	if initialize {
		inventory, err = s.filterEntitiesForDomains(ctx, s.o.Kinds.ContentFilters.Domains)
	} else {
		inventory, err = s.filterEntities(ctx)
	}
	if err != nil {
		return err
	}
	allowed := inventory.allowed()
	patch := map[string]string{}
	seen := map[string]bool{}
	for _, f := range choices {
		value, err := f.value()
		if err != nil {
			return err
		}
		if seen[f.EntityID] {
			return fmt.Errorf("%w: entity %q is set twice", ErrInvalid, f.EntityID)
		}
		seen[f.EntityID] = true
		if !allowed[f.EntityID] {
			if initialize {
				continue
			}
			return fmt.Errorf("%w: entity %q is not a content filter on this domain", ErrInvalid, f.EntityID)
		}
		patch[readerprefs.Key(f.EntityID)] = value
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	conflict := `DO UPDATE SET overrides=saved.overrides || EXCLUDED.overrides,include_deleted=COALESCE($4::boolean,saved.include_deleted),updated_at=now()`
	if initialize {
		conflict = `DO NOTHING`
	}
	_, err = s.o.Pool.Exec(ctx, `INSERT INTO `+s.t.prefs+` AS saved(tenant_id,user_id,overrides,include_deleted,updated_at) VALUES($1,$2,$3,COALESCE($4::boolean,false),now()) ON CONFLICT(tenant_id,user_id) `+conflict, s.o.Tenant, actor.AccountID(), raw, includeDeleted)
	return err
}
