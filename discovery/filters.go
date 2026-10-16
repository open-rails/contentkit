package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/readerprefs"
	"github.com/open-rails/contentkit/kinds"
)

// ContentFilterState is one configured content filter as the reader has it.
type ContentFilterState struct {
	Group groups.GroupCard `json:"entity"`
	// Label is the configured label in the requested language, else the
	// group's name.
	Label          string `json:"label"`
	DefaultVisible bool   `json:"default_visible"`
	// Visible is the reader's setting, Overridden whether it is their own.
	Visible    bool `json:"visible"`
	Overridden bool `json:"overridden"`
}

// ContentFilters are the reader's content filters, applied in every read.
type ContentFilters struct {
	Filters []ContentFilterState `json:"filters"`
	// IncludeDeleted lists deleted content the reader holds.
	IncludeDeleted bool `json:"include_deleted"`
}

// ContentFiltersUpdate replaces the reader's settings.
type ContentFiltersUpdate struct {
	Filters        []FilterSetting `json:"filters"`
	IncludeDeleted bool            `json:"include_deleted"`
}

// ContentFiltersInitialization binds guest preference adoption to the account
// just registered, even if the client signs into another account before retry.
type ContentFiltersInitialization struct {
	AccountID      string          `json:"account_id"`
	Filters        []FilterSetting `json:"filters"`
	IncludeDeleted bool            `json:"include_deleted"`
}

// FilterSetting shows or hides one configured filter's group.
type FilterSetting struct {
	GroupID string `json:"group_id"`
	Visible bool   `json:"visible"`
}

// Overrides are stored as content_filter_prefs.overrides: group id to show
// or hide.
const (
	show = "show"
	hide = "hide"
)

// filterGroups resolves the configured filters' groups to their ids.
func (s *Service) filterGroups(ctx context.Context) (map[kinds.GroupRef]string, error) {
	out := map[kinds.GroupRef]string{}
	fs := s.o.Kinds.ContentFilters
	if len(fs) == 0 {
		return out, nil
	}
	ks, ss := make([]string, len(fs)), make([]string, len(fs))
	for i, f := range fs {
		ks[i], ss[i] = f.Group.Kind, f.Group.Slug
	}
	rows, err := s.o.Pool.Query(ctx, `SELECT g.id, g.kind, g.slug FROM `+s.t.groups+` g JOIN unnest($2::text[], $3::text[]) AS r(kind, slug)
 ON g.kind = r.kind AND g.slug = r.slug WHERE g.tenant_id = $1 AND g.state = 'active' AND g.scope_id = ''`, s.o.Tenant, ks, ss)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var ref kinds.GroupRef
		if err := rows.Scan(&id, &ref.Kind, &ref.Slug); err != nil {
			return nil, err
		}
		out[ref] = id
	}
	return out, rows.Err()
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

// ContentFilters is the current reader's content filters: every
// configured filter whose group exists, labeled in language. Defaults come
// from the request's site; outside a request the first site's defaults apply.
func (s *Service) ContentFilters(ctx context.Context, actor access.Actor, language string) (ContentFilters, error) {
	out := ContentFilters{Filters: []ContentFilterState{}}
	if language != "" {
		if err := s.checkLanguage(language); err != nil {
			return out, err
		}
	}
	fgs, err := s.filterGroups(ctx)
	if err != nil {
		return out, err
	}
	overrides := map[string]string{}
	if actor.Member() {
		overrides, out.IncludeDeleted, err = s.prefs(ctx, actor.AccountID())
		if err != nil {
			return out, err
		}
	} else if actor.Anonymous {
		allowed := make(map[string]bool, len(fgs))
		for _, id := range fgs {
			allowed[id] = true
		}
		choices, err := readerprefs.Overrides(ctx, allowed)
		if err != nil {
			return out, fmt.Errorf("%w: %s", ErrInvalid, err)
		}
		for id, visible := range choices {
			overrides[id] = hide
			if visible {
				overrides[id] = show
			}
		}
	}
	ids := make([]groups.GroupID, 0, len(fgs))
	for _, id := range fgs {
		ids = append(ids, groups.GroupID(id))
	}
	cards, err := s.o.Groups.Cards(ctx, ids, groups.CardOptions{Language: language})
	if err != nil {
		return out, err
	}
	byID := make(map[string]groups.GroupCard, len(cards))
	for _, c := range cards {
		byID[string(c.ID)] = c
	}
	lang, _ := s.displayLanguage(language)
	site, _ := s.o.Kinds.SiteFor(gate.Site(ctx))
	for _, f := range s.o.Kinds.ContentFilters {
		id := fgs[f.Group]
		g, ok := byID[id]
		if !ok {
			continue
		}
		label := f.Labels[lang]
		if label == "" {
			label = g.Name
		}
		o, overridden := overrides[id]
		defaultVisible := site.DefaultVisible(f)
		visible := defaultVisible
		if overridden {
			visible = o != hide
		}
		out.Filters = append(out.Filters, ContentFilterState{Group: g, Label: label, DefaultVisible: defaultVisible, Visible: visible, Overridden: overridden})
	}
	return out, nil
}

// SetContentFilters replaces the signed-in reader's settings; each names a
// configured filter's group.
func (s *Service) SetContentFilters(ctx context.Context, actor access.Actor, u ContentFiltersUpdate) error {
	return s.setContentFilters(ctx, actor, u, false)
}

// InitializeContentFilters adopts a new account's guest choices only when no
// account settings exist. Retries and delayed registration callbacks cannot
// overwrite settings the account has already saved.
func (s *Service) InitializeContentFilters(ctx context.Context, actor access.Actor, u ContentFiltersInitialization) error {
	if u.AccountID == "" {
		return fmt.Errorf("%w: account_id is required", ErrInvalid)
	}
	if !actor.Member() || actor.AccountID() != u.AccountID {
		return auth.ErrForbidden
	}
	return s.setContentFilters(ctx, actor, ContentFiltersUpdate{Filters: u.Filters, IncludeDeleted: u.IncludeDeleted}, true)
}

func (s *Service) setContentFilters(ctx context.Context, actor access.Actor, u ContentFiltersUpdate, initialize bool) error {
	if !actor.Member() {
		return fmt.Errorf("%w: content filters belong to a signed-in reader", ErrInvalid)
	}
	fgs, err := s.filterGroups(ctx)
	if err != nil {
		return err
	}
	valid := map[string]bool{}
	for _, id := range fgs {
		valid[id] = true
	}
	overrides := map[string]string{}
	for _, f := range u.Filters {
		if !valid[f.GroupID] {
			return fmt.Errorf("%w: group %q is not a content filter", ErrInvalid, f.GroupID)
		}
		if _, dup := overrides[f.GroupID]; dup {
			return fmt.Errorf("%w: group %q is set twice", ErrInvalid, f.GroupID)
		}
		overrides[f.GroupID] = hide
		if f.Visible {
			overrides[f.GroupID] = show
		}
	}
	raw, err := json.Marshal(overrides)
	if err != nil {
		return err
	}
	conflict := `DO UPDATE SET overrides = EXCLUDED.overrides, include_deleted = EXCLUDED.include_deleted, updated_at = now()`
	if initialize {
		conflict = `DO NOTHING`
	}
	_, err = s.o.Pool.Exec(ctx, `INSERT INTO `+s.t.prefs+` (tenant_id, user_id, overrides, include_deleted, updated_at) VALUES ($1, $2, $3, $4, now())
 ON CONFLICT (tenant_id, user_id) `+conflict,
		s.o.Tenant, actor.AccountID(), raw, u.IncludeDeleted)
	return err
}
