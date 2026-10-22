package catalog

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
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
	// Slugs are decorative and always follow the title; URLs resolve by code.
	t.Slug = codes.Slugify(t.Title)
	aliases := make([]string, 0, len(t.Aliases))
	for _, a := range t.Aliases {
		if a = strings.TrimSpace(a); a != "" && utf8.RuneCountInString(a) <= maxTitle && !slices.Contains(aliases, a) {
			aliases = append(aliases, a)
		}
	}
	t.Aliases = aliases
	return t, nil
}

func checkComments(c string) error {
	switch c {
	case content.CommentsOpen, content.CommentsClosed, content.CommentsHidden:
		return nil
	}
	return invalid("comments %q must be open, closed or hidden", c)
}

// checkRule validates r against the config (access.Rule.Validate) and sorts
// its tiers; its group is checked against storage on write (checkRuleGroups).
func (s *Store) checkRule(r access.Rule) (access.Rule, error) {
	if err := r.Validate(s.cfg); err != nil {
		return r, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	r.Tiers = slices.Sorted(slices.Values(r.Tiers))
	return r, nil
}

// defaultRule keeps public access as the default when permitted. A restricted
// kind instead starts with its first permitted level; group access still
// requires the caller to name its group explicitly.
func (s *Store) defaultRule(kind string) access.Rule {
	k := s.cfg.Kinds[kind]
	r := access.Rule{Level: access.Public}
	if !k.AllowsAccess(string(access.Public)) && len(k.AccessLevels) > 0 {
		r.Level = access.Level(k.AccessLevels[0])
		if r.Level == access.TierLevel && len(s.cfg.Tiers) > 0 {
			r.Tiers = []string{s.cfg.Tiers[0]}
		}
	}
	return r
}

func (s *Store) defaultComments(kind string) string {
	k := s.cfg.Kinds[kind]
	if !k.AllowsComments(content.CommentsOpen) && len(k.CommentModes) > 0 {
		return k.CommentModes[0]
	}
	return content.CommentsOpen
}

// checkKindRule applies the host's rule policy after ordinary rule validation.
func (s *Store) checkKindRule(kind string, r access.Rule) error {
	k, err := s.kind(kind)
	if err != nil {
		return err
	}
	if !k.AllowsAccess(string(r.Level)) {
		return invalid("kind %s does not allow %s access", kind, r.Level)
	}
	return nil
}

func (s *Store) checkKindComments(kind, mode string) error {
	k, err := s.kind(kind)
	if err != nil {
		return err
	}
	if !k.AllowsComments(mode) {
		return invalid("kind %s does not allow %s comments", kind, mode)
	}
	return nil
}

// checkStoredPolicies prevents publication/restoration from bypassing a host
// policy tightened since the item was created. The caller holds content locks.
func (s *Store) checkStoredPolicies(ctx context.Context, tx pgx.Tx, ids []string) error {
	rows, err := tx.Query(ctx, `SELECT c.kind, c.comments, c.attributes, c.access, v.access FROM `+s.t.items+` c
 LEFT JOIN `+s.t.versions+` v ON v.tenant_id=c.tenant_id AND v.content_id=c.id AND v.state <> 'deleted'
 WHERE c.tenant_id=$1 AND c.id=ANY($2)`, s.tenant, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, comments string
		var attributes, itemRule, versionRule []byte
		if err := rows.Scan(&kind, &comments, &attributes, &itemRule, &versionRule); err != nil {
			return err
		}
		var values map[string]any
		if err := json.Unmarshal(attributes, &values); err != nil {
			return err
		}
		if err := kinds.ValidateRequiredAttributes(s.cfg.Kinds[kind].Attributes, values); err != nil {
			return invalid("%v", err)
		}
		if err := s.checkKindComments(kind, comments); err != nil {
			return err
		}
		for _, raw := range [][]byte{itemRule, versionRule} {
			if raw == nil {
				continue
			}
			var rule access.Rule
			if err := json.Unmarshal(raw, &rule); err != nil {
				return err
			}
			if err := s.checkKindRule(kind, rule); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}

// checkRuleGroups refuses a rule naming a group that is not an active group of
// a sellable kind.
func (s *Store) checkRuleGroups(ctx context.Context, q Querier, rules ...*access.Rule) error {
	var ids []string
	for _, r := range rules {
		if r != nil && r.Group != "" && !slices.Contains(ids, r.Group) {
			ids = append(ids, r.Group)
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
	anchored := len(ss) > 0 && ss[0].StartPage != ""
	seen := map[string]bool{}
	for i, sg := range ss {
		if (sg.StartPage != "") != anchored {
			return nil, invalid("chapters cannot mix page starts and numeric ranges")
		}
		if anchored {
			digest, err := hex.DecodeString(sg.StartPage)
			if seen[sg.StartPage] || err != nil || len(digest) != 32 || strings.ToLower(sg.StartPage) != sg.StartPage {
				return nil, invalid("chapter %d: invalid or duplicate starting page", i+1)
			}
			seen[sg.StartPage] = true
			if sg.Start != 0 || sg.End != 0 {
				return nil, invalid("chapter %d: page starts do not take numeric ranges", i+1)
			}
		} else if sg.Start < 0 || sg.End <= sg.Start {
			return nil, invalid("segment %d: start must be >= 0 and end > start", i)
		}
		if !anchored && i > 0 && sg.Start < out[i-1].End {
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

func checkSegmentInputs(in []SegmentInput) ([]Segment, error) {
	segments := make([]Segment, len(in))
	for i, sg := range in {
		segments[i] = Segment{Position: sg.Position, Title: sg.Title, StartPage: sg.StartPage, Start: sg.Start, End: sg.End, ReleaseAt: sg.ReleaseAt}
	}
	return checkSegments(segments)
}
