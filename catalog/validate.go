package catalog

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/open-rails/contentkit/internal/codes"
	"github.com/open-rails/contentkit/kinds"
)

// Text bounds, in characters.
const (
	maxTitle       = 512
	maxDescription = 10_000
	maxBody        = 200_000
	maxAliases     = 64
	maxLabel       = 128
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func (s *Store) kind(name string) (kinds.Kind, error) {
	k, ok := s.cfg.Kinds[name]
	if !ok {
		return kinds.Kind{}, invalid("unknown kind %q", name)
	}
	return k, nil
}

func (s *Store) checkLanguage(l string) error {
	if !slices.Contains(s.cfg.Languages, l) {
		return invalid("language %q is not one of %v", l, s.cfg.Languages)
	}
	return nil
}

// cleanText validates t and sanitizes its body; the slug defaults to the
// title's.
func (s *Store) cleanText(ctx context.Context, language string, t Text) (Text, error) {
	if err := s.checkLanguage(language); err != nil {
		return t, err
	}
	t.Title = strings.TrimSpace(t.Title)
	if t.Title == "" {
		return t, invalid("text %s: title is required", language)
	}
	if utf8.RuneCountInString(t.Title) > maxTitle || utf8.RuneCountInString(t.Description) > maxDescription ||
		utf8.RuneCountInString(t.Body) > maxBody || len(t.Aliases) > maxAliases {
		return t, invalid("text %s exceeds its limits", language)
	}
	body, err := s.body.Sanitize(ctx, t.Body)
	if err != nil {
		return t, err
	}
	t.Body = body
	t.Description = strings.TrimSpace(t.Description)
	t.Slug = strings.TrimSpace(t.Slug)
	if t.Slug == "" {
		t.Slug = codes.Slugify(t.Title)
	} else if t.Slug = codes.Slugify(t.Slug); t.Slug == "" {
		return t, invalid("text %s: slug has no letters or digits", language)
	}
	aliases := make([]string, 0, len(t.Aliases))
	for _, a := range t.Aliases {
		if a = strings.TrimSpace(a); a != "" && utf8.RuneCountInString(a) <= maxTitle && !slices.Contains(aliases, a) {
			aliases = append(aliases, a)
		}
	}
	t.Aliases = aliases
	return t, nil
}

func (s *Store) checkListing(l string) error {
	switch l {
	case ListingListed, ListingMembers, ListingUnlisted:
		return nil
	}
	return invalid("listing %q must be listed, members or unlisted", l)
}

// checkRule validates r's shape; its group is checked against storage on
// write (checkRuleGroups).
func (s *Store) checkRule(r Rule) (Rule, error) {
	switch r.Level {
	case LevelPublic, LevelPPV:
		if len(r.Tiers) > 0 || r.GroupID != "" {
			return r, invalid("access %s takes no tiers or group", r.Level)
		}
	case LevelTier:
		if len(r.Tiers) == 0 || r.GroupID != "" {
			return r, invalid("access tier names one or more tiers and no group")
		}
		for _, t := range r.Tiers {
			if !slices.Contains(s.cfg.Tiers, t) {
				return r, invalid("tier %q is not one of %v", t, s.cfg.Tiers)
			}
		}
		r.Tiers = slices.Compact(slices.Sorted(slices.Values(r.Tiers)))
	case LevelGroup, LevelGroupPPV:
		if r.GroupID == "" || len(r.Tiers) > 0 {
			return r, invalid("access %s names a group and no tiers", r.Level)
		}
	default:
		return r, invalid("access level %q must be public, tier, group, ppv or group_ppv", r.Level)
	}
	return r, nil
}

// checkRuleGroups refuses a rule naming a group that is not an active group of
// a sellable kind.
func (s *Store) checkRuleGroups(ctx context.Context, q Querier, rules ...*Rule) error {
	var ids []string
	for _, r := range rules {
		if r != nil && r.GroupID != "" && !slices.Contains(ids, r.GroupID) {
			ids = append(ids, r.GroupID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `SELECT id, kind FROM `+s.t.groups+` WHERE tenant_id = $1 AND id = ANY($2) AND state = 'active'`, s.tenant, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := map[string]string{}
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return err
		}
		found[id] = kind
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		kind, ok := found[id]
		if !ok {
			return invalid("access group %s does not exist", id)
		}
		if !s.cfg.GroupKinds[kind].Sellable {
			return invalid("access group %s is a %s, which is not sellable", id, kind)
		}
	}
	return nil
}

func (s *Store) checkRestrictions(rs []string) ([]string, error) {
	out := []string{}
	for _, r := range rs {
		if !slices.Contains(s.cfg.Restrictions, r) {
			return nil, invalid("restriction %q is not one of %v", r, s.cfg.Restrictions)
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (s *Store) checkTraits(k kinds.Kind, kind string, ts []string) ([]string, error) {
	out := []string{}
	for _, t := range ts {
		if !slices.Contains(k.Versions.Traits, t) {
			return nil, invalid("trait %q is not declared for %s", t, kind)
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (s *Store) checkVersionLanguages(k kinds.Kind, ls []string) ([]string, error) {
	out := []string{}
	for _, l := range ls {
		if err := s.checkLanguage(l); err != nil {
			return nil, err
		}
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	switch {
	case len(out) == 0:
		return nil, invalid("a version serves at least one language")
	case k.Versions.Languages == "one" && len(out) > 1:
		return nil, invalid("a version of this kind serves one language")
	}
	slices.Sort(out)
	return out, nil
}

// checkAttributes validates attrs against the declared ones and normalizes
// each value (kinds.ValidateAttributes).
func checkAttributes(declared map[string]kinds.Attribute, attrs map[string]any) (map[string]any, error) {
	out, err := kinds.ValidateAttributes(declared, attrs)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return out, nil
}

func (s *Store) checkCreators(cs []Creator) ([]Creator, error) {
	out := make([]Creator, 0, len(cs))
	for i, c := range cs {
		c.UserID = strings.TrimSpace(c.UserID)
		if c.Role = strings.TrimSpace(c.Role); c.Role == "" {
			c.Role = "author"
		}
		if c.UserID == "" || len(c.UserID) > 255 || len(c.Role) > 64 {
			return nil, invalid("a creator needs a user_id and a role of at most 64 bytes")
		}
		for _, o := range out {
			if o.UserID == c.UserID && o.Role == c.Role {
				return nil, invalid("creator %s is listed twice as %s", c.UserID, c.Role)
			}
		}
		c.Position = i
		out = append(out, c)
	}
	return out, nil
}

func checkSegments(ss []Segment) ([]Segment, error) {
	out := make([]Segment, len(ss))
	for i, sg := range ss {
		if sg.Start < 0 || sg.End <= sg.Start {
			return nil, invalid("segment %d: start must be >= 0 and end > start", i)
		}
		if i > 0 && sg.Start < out[i-1].End {
			return nil, invalid("segment %d overlaps the one before", i)
		}
		if utf8.RuneCountInString(sg.Title) > maxTitle {
			return nil, invalid("segment %d: title too long", i)
		}
		sg.Position, sg.Released = i, false
		out[i] = sg
	}
	return out, nil
}
